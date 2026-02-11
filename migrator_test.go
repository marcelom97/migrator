package migrator

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

//go:embed testdata/*.sql
var testMigrationsEmbed embed.FS

//go:embed testdata/invalid/*.sql
var invalidTestMigrationsEmbed embed.FS

func testMigrationsFS(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(testMigrationsEmbed, "testdata")
	if err != nil {
		t.Fatalf("failed to create sub FS: %v", err)
	}
	return sub
}

func invalidMigrationsFS(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(invalidTestMigrationsEmbed, "testdata/invalid")
	if err != nil {
		t.Fatalf("failed to create sub FS: %v", err)
	}
	return sub
}

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

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		var exists bool
		if err := db.QueryRow(fmt.Sprintf(`
			SELECT EXISTS (
				SELECT FROM pg_tables
				WHERE schemaname = '%s'
				AND tablename = 'schema_migrations'
			);
		`, schema)).Scan(&exists); err != nil {
			t.Fatalf("failed to check if migrations table exists: %v", err)
		}
		if !exists {
			t.Fatal("migrations table does not exist")
		}
	})

	t.Run("applies migrations in order", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
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

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
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

		m, err := New(db, invalidMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
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
					m, err := New(db, testMigrationsFS(t))
					if err != nil {
						done <- err
						return
					}
					done <- m.Run(context.Background())
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

func TestNewValidation(t *testing.T) {
	t.Run("nil db returns error", func(t *testing.T) {
		_, err := New(nil, testMigrationsFS(t))
		if err == nil {
			t.Fatal("expected error for nil db, got nil")
		}
	})

	t.Run("nil migrations returns error", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		_, err := New(db, nil)
		if err == nil {
			t.Fatal("expected error for nil migrations, got nil")
		}
	})
}

func TestOptions(t *testing.T) {
	t.Run("custom table name", func(t *testing.T) {
		db, schema, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t), WithTableName("custom_migrations"))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		var exists bool
		if err := db.QueryRow(fmt.Sprintf(`
			SELECT EXISTS (
				SELECT FROM pg_tables
				WHERE schemaname = '%s'
				AND tablename = 'custom_migrations'
			);
		`, schema)).Scan(&exists); err != nil {
			t.Fatalf("failed to check table: %v", err)
		}
		if !exists {
			t.Fatal("custom_migrations table does not exist")
		}
	})

	t.Run("custom lock id", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t), WithLockID(99999))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}
	})

	t.Run("with logger", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
		m, err := New(db, testMigrationsFS(t), WithLogger(logger))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}
	})
}

func TestRunWithCancelledContext(t *testing.T) {
	db, _, closeDB := openDB(t)
	defer closeDB()

	m, err := New(db, testMigrationsFS(t))
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Run(ctx); err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
