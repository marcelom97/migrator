package migrator

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

//go:embed testdata/invalid/*.sql
var invalidTestMigrations embed.FS

func openDB(t *testing.T) (*sql.DB, string, func()) {
	t.Helper()
	now := time.Now().UnixNano()
	db, err := sql.Open("postgres", fmt.Sprintf("%s?sslmode=disable&search_path=test_%d", os.Getenv("DATABASE_URL"), now))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	schema := fmt.Sprintf(`
	CREATE SCHEMA IF NOT EXISTS test_%d;
	SET search_path TO test_%d, public;
	`, now, now)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}
	return db, fmt.Sprintf("test_%d", now), func() {
		query := fmt.Sprintf("DROP SCHEMA test_%d CASCADE;", now)
		if _, err := db.Exec(query); err != nil {
			t.Fatalf("failed to drop schema: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("failed to close database: %v", err)
		}
	}
}

func TestMigrator(t *testing.T) {
	t.Run("creates migrations table", func(t *testing.T) {
		db, schema, closeDB := openDB(t)
		defer closeDB()
		m := New(db, testMigrations)
		if err := m.tx(func(tx *sql.Tx) error {
			if err := m.createMigrationsTable(tx); err != nil {
				return fmt.Errorf("failed to create migrations table: %w", err)
			}
			var exists bool
			if err := tx.QueryRow(fmt.Sprintf(`
			SELECT EXISTS (
				SELECT FROM pg_tables
				WHERE schemaname = '%s'
				AND tablename = 'schema_migrations'
			);
		`, schema)).Scan(&exists); err != nil {
				return fmt.Errorf("failed to check if migrations table exists: %w", err)
			}
			if !exists {
				return fmt.Errorf("migrations table does not exist")
			}
			return nil
		}); err != nil {
			t.Fatalf("failed to create migrations table: %v", err)
		}
	})

	t.Run("applies migrations in order", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()
		m := New(db, testMigrations)
		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
		if err != nil {
			t.Fatalf("failed to get applied migrations: %v", err)
		}
		defer rows.Close()

		var versions []string
		for rows.Next() {
			var version string
			if err := rows.Scan(&version); err != nil {
				t.Fatalf("failed to scan migration version: %v", err)
			}
			versions = append(versions, version)
		}

		expectedVersions := []string{
			"001_create_test_table",
			"002_add_test_column",
		}
		if len(versions) != len(expectedVersions) {
			t.Fatalf("expected %d migrations, got %d", len(expectedVersions), len(versions))
		}
		for i, version := range versions {
			if version != expectedVersions[i] {
				t.Fatalf("expected migration %s, got %s", expectedVersions[i], version)
			}
		}

		var exists bool
		if err := db.QueryRow(`
			SELECT EXISTS (
				SELECT FROM information_schema.columns
				WHERE table_name = 'test_table'
				AND column_name = 'test_column'
			);
		`).Scan(&exists); err != nil {
			t.Fatalf("failed to check if test_column exists: %v", err)
		}

		if !exists {
			t.Fatal("test_column does not exist")
		}
	})

	t.Run("skips already applied migrations", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()
		m := New(db, testMigrations)

		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}
		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
			t.Fatalf("failed to get applied migrations count: %v", err)
		}
		if count != 2 {
			t.Fatalf("expected 2 applied migrations, got %d", count)
		}
	})

	t.Run("handles invalid migration files", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m := New(db, invalidTestMigrations)
		if err := m.Run(); err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestConcurrentMigrations(t *testing.T) {
	tests := []struct {
		name            string
		instances       int
		expectedSuccess int
		expectedLocks   int
	}{
		{
			name:            "two concurrent instances",
			instances:       2,
			expectedSuccess: 1,
			expectedLocks:   1,
		},
		{
			name:            "five concurrent instances",
			instances:       5,
			expectedSuccess: 1,
			expectedLocks:   4,
		},
		{
			name:            "ten concurrent instances",
			instances:       10,
			expectedSuccess: 1,
			expectedLocks:   9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _, closeDB := openDB(t)
			defer closeDB()

			done := make(chan error, tt.instances)

			for i := 0; i < tt.instances; i++ {
				go func() {
					m := New(db, testMigrations)
					done <- m.Run()
				}()
			}

			var (
				successCount int
				lockCount    int
			)

			for i := 0; i < tt.instances; i++ {
				err := <-done
				if err == nil {
					successCount++
				} else if err.Error() == "another migration is in progress" {
					lockCount++
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}

			if successCount != tt.expectedSuccess {
				t.Errorf("expected %d successful migrations, got %d", tt.expectedSuccess, successCount)
			}
			if lockCount != tt.expectedLocks {
				t.Errorf("expected %d lock failures, got %d", tt.expectedLocks, lockCount)
			}

			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
				t.Fatalf("failed to get applied migrations count: %v", err)
			}
			if count != 2 {
				t.Fatalf("expected 2 applied migrations, got %d", count)
			}
		})
	}
}
