package migrator

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type Migrator struct {
	db         *sql.DB
	migrations fs.FS
}

// New creates a new Migrator instance with embedded migrations
func New(db *sql.DB, migrations fs.FS) *Migrator {
	return &Migrator{
		db:         db,
		migrations: migrations,
	}
}

func (m *Migrator) tx(fun func(tx *sql.Tx) error) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	if err := fun(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func (m *Migrator) tryLock() (bool, error) {
	var locked bool
	err := m.db.QueryRow(`SELECT pg_try_advisory_lock(1)`).Scan(&locked)
	if err != nil {
		return false, fmt.Errorf("failed to acquire advisory lock: %w", err)
	}
	return locked, nil
}

func (m *Migrator) unlock() error {
	var released bool
	err := m.db.QueryRow(`SELECT pg_advisory_unlock(1)`).Scan(&released)
	if err != nil {
		return fmt.Errorf("failed to release advisory lock: %w", err)
	}
	return nil
}

// Run applies all pending migrations
func (m *Migrator) Run() error {
	locked, err := m.tryLock()
	if err != nil {
		return fmt.Errorf("failed to acquire advisory lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("another migration is in progress")
	}
	defer func() {
		if locked {
			if err := m.unlock(); err != nil {
				fmt.Printf("failed to release advisory lock: %v\n", err)
			}
		}
	}()

	return m.tx(func(tx *sql.Tx) error {
		if err := m.createMigrationsTable(tx); err != nil {
			return fmt.Errorf("failed to create migrations table: %w", err)
		}

		if _, err := tx.Exec(`LOCK TABLE schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
			return fmt.Errorf("failed to lock schema_migrations: %w", err)
		}

		applied, err := m.getAppliedMigrations(tx)
		if err != nil {
			return fmt.Errorf("failed to get applied migrations: %w", err)
		}

		files, err := m.getMigrationFiles()
		if err != nil {
			return fmt.Errorf("failed to get migration files: %w", err)
		}

		for _, file := range files {
			version := strings.TrimSuffix(filepath.Base(file), ".sql")
			if !applied[version] {
				if err := m.applyMigration(tx, version, file); err != nil {
					return fmt.Errorf("failed to apply migration %s: %w", version, err)
				}
				fmt.Printf("Applied migration: %s\n", version)
			}
		}

		return nil
	})
}

func (m *Migrator) createMigrationsTable(tx *sql.Tx) error {
	query := `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`

	_, err := tx.Exec(query)
	return err
}

func (m *Migrator) getAppliedMigrations(tx *sql.Tx) (map[string]bool, error) {
	applied := make(map[string]bool)

	rows, err := tx.Query("SELECT version FROM schema_migrations")
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
	var files []string

	err := fs.WalkDir(m.migrations, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".sql") {
			files = append(files, path)
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to walk migrations directory: %w", err)
	}

	sort.Strings(files)
	return files, nil
}

func (m *Migrator) applyMigration(tx *sql.Tx, version string, file string) error {
	content, err := fs.ReadFile(m.migrations, file)
	if err != nil {
		return fmt.Errorf("failed to read migration file: %w", err)
	}

	if _, err := tx.Exec(string(content)); err != nil {
		return err
	}

	if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
		return err
	}

	return nil
}
