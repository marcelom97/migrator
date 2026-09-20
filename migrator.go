package migrator

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// ErrLockNotAcquired is returned by Run when another migration holds the advisory lock.
var ErrLockNotAcquired = errors.New("another migration is in progress")

// Migrator applies SQL migrations to a PostgreSQL database.
type Migrator struct {
	db         *sql.DB
	migrations fs.FS
	cfg        config
}

// New creates a new Migrator. Returns an error if db or migrations is nil.
func New(db *sql.DB, migrations fs.FS, opts ...Option) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("migrator: db must not be nil")
	}
	if migrations == nil {
		return nil, errors.New("migrator: migrations must not be nil")
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	return &Migrator{
		db:         db,
		migrations: migrations,
		cfg:        cfg,
	}, nil
}

// discardConn drops the session rather than pooling it. Close alone would hand
// the next caller a session that may still hold the advisory lock.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

func (m *Migrator) tryLock(ctx context.Context, conn *sql.Conn) (bool, error) {
	var locked bool
	err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, m.cfg.lockID).Scan(&locked)
	if err != nil {
		// The lock may have been granted before the error reached us.
		discardConn(conn)
		return false, fmt.Errorf("failed to acquire advisory lock: %w", err)
	}
	return locked, nil
}

// acquireLock takes the advisory lock, waiting up to cfg.lockTimeout for a
// concurrent migration to release it.
func (m *Migrator) acquireLock(ctx context.Context, conn *sql.Conn) error {
	locked, err := m.tryLock(ctx, conn)
	if err != nil {
		return err
	}
	if locked {
		return nil
	}
	if m.cfg.lockTimeout <= 0 {
		return ErrLockNotAcquired
	}

	const maxBackoff = time.Second
	deadline := time.Now().Add(m.cfg.lockTimeout)
	backoff := 50 * time.Millisecond

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrLockNotAcquired
		}
		wait := min(backoff, remaining)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		locked, err := m.tryLock(ctx, conn)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (m *Migrator) unlock(ctx context.Context, conn *sql.Conn) error {
	var released bool
	err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, m.cfg.lockID).Scan(&released)
	if err != nil {
		// The unlock may not have run.
		discardConn(conn)
		return fmt.Errorf("failed to release advisory lock: %w", err)
	}
	if !released {
		return errors.New("advisory lock was not held")
	}
	return nil
}

// Run applies all pending migrations within a single transaction.
func (m *Migrator) Run(ctx context.Context) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire database connection: %w", err)
	}
	defer conn.Close()

	if err := m.acquireLock(ctx, conn); err != nil {
		return err
	}
	defer func() {
		if err := m.unlock(context.Background(), conn); err != nil {
			m.cfg.logger.Error("failed to release advisory lock", "error", err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := m.createMigrationsTable(ctx, tx); err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	lockQuery := fmt.Sprintf(`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`, m.cfg.tableName)
	if _, err := tx.ExecContext(ctx, lockQuery); err != nil {
		return fmt.Errorf("failed to lock %s: %w", m.cfg.tableName, err)
	}

	applied, err := m.getAppliedMigrations(ctx, tx)
	if err != nil {
		return fmt.Errorf("failed to get applied migrations: %w", err)
	}

	files, err := m.getMigrationFiles()
	if err != nil {
		return fmt.Errorf("failed to get migration files: %w", err)
	}

	for _, file := range files {
		version := strings.TrimSuffix(file, ".sql")
		if !applied[version] {
			if err := m.applyMigration(ctx, tx, version, file); err != nil {
				return fmt.Errorf("failed to apply migration %s: %w", version, err)
			}
			m.cfg.logger.Info("applied migration", "version", version)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migrations: %w", err)
	}

	return nil
}

func (m *Migrator) createMigrationsTable(ctx context.Context, tx *sql.Tx) error {
	query := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`, m.cfg.tableName)

	_, err := tx.ExecContext(ctx, query)
	return err
}

func (m *Migrator) getAppliedMigrations(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	applied := make(map[string]bool)

	query := fmt.Sprintf("SELECT version FROM %s", m.cfg.tableName)
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}

	return applied, rows.Err()
}

func (m *Migrator) getMigrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(m.migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory: %w", err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}

	sort.Strings(files)
	return files, nil
}

func (m *Migrator) applyMigration(ctx context.Context, tx *sql.Tx, version string, file string) error {
	content, err := fs.ReadFile(m.migrations, file)
	if err != nil {
		return fmt.Errorf("failed to read migration file: %w", err)
	}

	if _, err := tx.ExecContext(ctx, string(content)); err != nil {
		return err
	}

	insertQuery := fmt.Sprintf("INSERT INTO %s (version) VALUES ($1)", m.cfg.tableName)
	if _, err := tx.ExecContext(ctx, insertQuery, version); err != nil {
		return err
	}

	return nil
}
