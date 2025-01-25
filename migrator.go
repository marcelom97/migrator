package migrator

import (
	"database/sql"
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

func (m *Migrator) Run() error {
	// Ensure migrations table exists
	if err := m.createMigrationsTable(); err != nil {
		return fmt.Errorf("failed to create migrations table: %w", err)
	}

	// Get applied migrations
	applied, err := m.getAppliedMigrations()
	if err != nil {
		return fmt.Errorf("failed to get applied migrations: %w", err)
	}

	// Get available migration files
	files, err := m.getMigrationFiles()
	if err != nil {
		return fmt.Errorf("failed to get migration files: %w", err)
	}

	// Run pending migrations
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")
		if !applied[version] {
			if err := m.applyMigration(version, file); err != nil {
				return fmt.Errorf("failed to apply migration %s: %w", version, err)
			}
			fmt.Printf("Applied migration: %s\n", version)
		}
	}

	return nil
}

func (m *Migrator) createMigrationsTable() error {
	query := `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`

	_, err := m.db.Exec(query)
	return err
}

func (m *Migrator) getAppliedMigrations() (map[string]bool, error) {
	applied := make(map[string]bool)

	rows, err := m.db.Query("SELECT version FROM schema_migrations")
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

	// Sort files by name to ensure correct order
	sort.Strings(files)
	return files, nil
}

func (m *Migrator) applyMigration(version string, file string) error {
	content, err := fs.ReadFile(m.migrations, file)
	if err != nil {
		return fmt.Errorf("failed to read migration file: %w", err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Execute migration
	if _, err := tx.Exec(string(content)); err != nil {
		return err
	}

	// Record migration
	if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
		return err
	}

	return tx.Commit()
}
