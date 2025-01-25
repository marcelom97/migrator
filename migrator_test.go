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
		if err := m.createMigrationsTable(); err != nil {
			t.Fatalf("failed to create migrations table: %v", err)
		}

		// Verify table exists
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
		m := New(db, testMigrations)
		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		// Verify migrations were applied
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

		// Verify all migrations were applied in order
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

		// Verify the actual schema changes
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

		// Run migrations twice
		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}
		if err := m.Run(); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		// Verify migrations were applied only once
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
		// Create a new migrator with invalid SQL
		m := New(db, invalidTestMigrations)
		if err := m.Run(); err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
