package migrator

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
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

// openDB gives each test its own schema. Advisory locks are keyed per database,
// not per schema, so tests sharing a lock id must not run in parallel.
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
		name      string
		instances int
	}{
		{name: "two concurrent instances", instances: 2},
		{name: "five concurrent instances", instances: 5},
		{name: "ten concurrent instances", instances: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _, closeDB := openDB(t)
			defer closeDB()

			// Shared instance: Run must be safe on one Migrator, not just on separate ones.
			m, err := New(db, testMigrationsFS(t))
			if err != nil {
				t.Fatalf("failed to create migrator: %v", err)
			}

			done := make(chan error, tt.instances)

			for i := 0; i < tt.instances; i++ {
				go func() {
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
				} else if errors.Is(err, ErrLockNotAcquired) {
					lockCount++
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}

			// An instance arriving after the commit finds nothing pending and also
			// succeeds, so the split between the two outcomes is timing-dependent.
			if successCount < 1 {
				t.Errorf("expected at least 1 successful migration, got %d", successCount)
			}
			if successCount+lockCount != tt.instances {
				t.Errorf("expected %d accounted results, got %d successes and %d lock failures",
					tt.instances, successCount, lockCount)
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

	t.Run("lock timeout waits for a concurrent migration", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t), WithLockTimeout(30*time.Second))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		const instances = 5
		done := make(chan error, instances)
		for i := 0; i < instances; i++ {
			go func() {
				done <- m.Run(context.Background())
			}()
		}

		for i := 0; i < instances; i++ {
			if err := <-done; err != nil {
				t.Errorf("expected every instance to acquire the lock, got: %v", err)
			}
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

// lockHeld reads pg_locks because pg_try_advisory_lock is re-entrant: a probe
// handed the holding session back out of the pool would report the lock free.
func lockHeld(t *testing.T, db *sql.DB, lockID int64) bool {
	t.Helper()

	var held bool
	if err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory'
			AND granted
			AND ((classid::bigint << 32) | objid::bigint) = $1
		)`, lockID).Scan(&held); err != nil {
		t.Fatalf("failed to probe advisory lock: %v", err)
	}
	return held
}

// waitLockReleased polls because the backend terminates asynchronously.
func waitLockReleased(t *testing.T, db *sql.DB, lockID int64) bool {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !lockHeld(t, db, lockID) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func holdLock(t *testing.T, db *sql.DB, lockID int64) *sql.Conn {
	t.Helper()

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed to acquire connection: %v", err)
	}

	var locked bool
	if err := conn.QueryRowContext(context.Background(),
		`SELECT pg_try_advisory_lock($1)`, lockID).Scan(&locked); err != nil {
		t.Fatalf("failed to acquire advisory lock: %v", err)
	}
	if !locked {
		t.Fatal("expected to acquire the advisory lock")
	}
	return conn
}

func TestDiscardConn(t *testing.T) {
	const lockID = int64(918273645)

	// The leak discardConn guards against; if this stops holding, the fix is moot.
	t.Run("close alone leaves the advisory lock held", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		conn := holdLock(t, db, lockID)
		if err := conn.Close(); err != nil {
			t.Fatalf("failed to close connection: %v", err)
		}

		if !lockHeld(t, db, lockID) {
			t.Fatal("expected the advisory lock to outlive Close; the leak this guards against is gone")
		}
	})

	t.Run("discard releases the advisory lock", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		conn := holdLock(t, db, lockID)

		// No Close: discardConn already closed the connection.
		discardConn(conn)

		if !waitLockReleased(t, db, lockID) {
			t.Fatal("expected the advisory lock to be released once the connection was discarded")
		}
	})

	// Run's path when the lock query fails: the session may hold the lock.
	t.Run("tryLock failure does not return the session to the pool", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t), WithLockID(lockID))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("failed to acquire connection: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := m.tryLock(ctx, conn); err == nil {
			t.Fatal("expected tryLock to fail on a cancelled context")
		}
		// Already discarded by tryLock.
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			t.Fatalf("unexpected error closing connection: %v", err)
		}

		if !waitLockReleased(t, db, lockID) {
			t.Fatal("expected no advisory lock to survive a failed tryLock")
		}
		// Pool must still be usable.
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("expected a later Run to succeed, got: %v", err)
		}
	})
}

func mapFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// reverseDirFS hands out directory entries in descending name order. MapFS and
// embed.FS both enumerate in ascending order, which would hide a missing sort.
type reverseDirFS struct{ fs.FS }

func (r reverseDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(r.FS, name)
	if err != nil {
		return nil, err
	}
	slices.Reverse(entries)
	return entries, nil
}

// noDB is a *sql.DB that never connects, for tests that touch no database.
func noDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()

	var exists bool
	if err := db.QueryRow(
		`SELECT EXISTS (SELECT FROM pg_tables WHERE schemaname = $1 AND tablename = $2)`,
		schema, table).Scan(&exists); err != nil {
		t.Fatalf("failed to check table %s: %v", table, err)
	}
	return exists
}

func appliedVersions(t *testing.T, db *sql.DB) []string {
	t.Helper()

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
	if err := rows.Err(); err != nil {
		t.Fatalf("failed to read applied migrations: %v", err)
	}
	return versions
}

func TestGetMigrationFiles(t *testing.T) {
	t.Run("selects and sorts .sql files", func(t *testing.T) {
		m, err := New(noDB(t), reverseDirFS{mapFS(map[string]string{
			"010_third.sql":  "SELECT 1;",
			"002_second.sql": "SELECT 1;",
			"001_first.sql":  "SELECT 1;",
		})})
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		files, err := m.getMigrationFiles()
		if err != nil {
			t.Fatalf("failed to get migration files: %v", err)
		}

		want := []string{"001_first.sql", "002_second.sql", "010_third.sql"}
		if !slices.Equal(files, want) {
			t.Fatalf("expected %v, got %v", want, files)
		}
	})

	t.Run("ignores directories and non-sql files", func(t *testing.T) {
		m, err := New(noDB(t), mapFS(map[string]string{
			"001_first.sql":      "SELECT 1;",
			"README.md":          "docs",
			"002_second.sql.bak": "SELECT 1;",
			"nested/003.sql":     "SELECT 1;",
			"004_dir.sql/x.txt":  "not a migration",
		}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		files, err := m.getMigrationFiles()
		if err != nil {
			t.Fatalf("failed to get migration files: %v", err)
		}

		want := []string{"001_first.sql"}
		if !slices.Equal(files, want) {
			t.Fatalf("expected %v, got %v", want, files)
		}
	})

	t.Run("reports an unreadable directory", func(t *testing.T) {
		m, err := New(noDB(t), os.DirFS(filepath.Join(t.TempDir(), "missing")))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		if _, err := m.getMigrationFiles(); err == nil {
			t.Fatal("expected error for a missing migrations directory, got nil")
		}
	})
}

func TestRunAtomicity(t *testing.T) {
	t.Run("a failed migration rolls back the whole run", func(t *testing.T) {
		db, schema, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, mapFS(map[string]string{
			"001_ok.sql":  "CREATE TABLE atomic_table (id INT);",
			"002_bad.sql": "THIS IS NOT VALID SQL;",
		}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for an invalid migration, got nil")
		}

		if tableExists(t, db, schema, "atomic_table") {
			t.Error("expected the first migration to be rolled back")
		}
		if tableExists(t, db, schema, "schema_migrations") {
			t.Error("expected the migrations table to be rolled back")
		}
	})

	t.Run("a failed run releases the advisory lock", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, invalidMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for an invalid migration, got nil")
		}

		if !waitLockReleased(t, db, defaultConfig().lockID) {
			t.Fatal("expected the advisory lock to be released after a failed run")
		}

		// A later run on the same pool must still work.
		ok, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := ok.Run(context.Background()); err != nil {
			t.Fatalf("expected a later run to succeed, got: %v", err)
		}
	})

	t.Run("a successful run releases the advisory lock", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		if !waitLockReleased(t, db, defaultConfig().lockID) {
			t.Fatal("expected the advisory lock to be released after a successful run")
		}
	})

	t.Run("cancelling mid-migration rolls back and releases the lock", func(t *testing.T) {
		db, schema, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, mapFS(map[string]string{
			"001_slow.sql": "CREATE TABLE slow_table (id INT); SELECT pg_sleep(30);",
		}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		if err := m.Run(ctx); err == nil {
			t.Fatal("expected error for a cancelled migration, got nil")
		}

		if tableExists(t, db, schema, "slow_table") {
			t.Error("expected the cancelled migration to be rolled back")
		}
		if !waitLockReleased(t, db, defaultConfig().lockID) {
			t.Fatal("expected the advisory lock to be released after a cancelled run")
		}
	})
}

func TestRunEdgeCases(t *testing.T) {
	t.Run("no migration files", func(t *testing.T) {
		db, schema, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, mapFS(map[string]string{}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("expected an empty migration set to succeed, got: %v", err)
		}

		if !tableExists(t, db, schema, "schema_migrations") {
			t.Error("expected the migrations table to be created")
		}
		if got := appliedVersions(t, db); len(got) != 0 {
			t.Errorf("expected no applied migrations, got %v", got)
		}
	})

	t.Run("new files are applied on a later run", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		first, err := New(db, mapFS(map[string]string{
			"001_first.sql": "CREATE TABLE incremental (id INT);",
		}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := first.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		second, err := New(db, mapFS(map[string]string{
			"001_first.sql":  "CREATE TABLE incremental (id INT);",
			"002_second.sql": "ALTER TABLE incremental ADD COLUMN name TEXT;",
		}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := second.Run(context.Background()); err != nil {
			t.Fatalf("failed to run migrations: %v", err)
		}

		want := []string{"001_first", "002_second"}
		if got := appliedVersions(t, db); !slices.Equal(got, want) {
			t.Fatalf("expected %v, got %v", want, got)
		}
	})

	t.Run("closed database", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		closeDB()

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for a closed database, got nil")
		}
	})
}

func TestLockTimeout(t *testing.T) {
	t.Run("expires while the lock is held", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		const lockID = int64(556677)
		holder := holdLock(t, db, lockID)
		defer discardConn(holder)

		m, err := New(db, testMigrationsFS(t), WithLockID(lockID), WithLockTimeout(300*time.Millisecond))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		start := time.Now()
		err = m.Run(context.Background())
		elapsed := time.Since(start)

		if !errors.Is(err, ErrLockNotAcquired) {
			t.Fatalf("expected ErrLockNotAcquired, got: %v", err)
		}
		if elapsed < 250*time.Millisecond {
			t.Errorf("expected Run to wait for the timeout, gave up after %s", elapsed)
		}
	})

	t.Run("cancelling while waiting returns the context error", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		const lockID = int64(667788)
		holder := holdLock(t, db, lockID)
		defer discardConn(holder)

		m, err := New(db, testMigrationsFS(t), WithLockID(lockID), WithLockTimeout(time.Minute))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()

		if err := m.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	})

	t.Run("proceeds once the lock is released", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		const lockID = int64(778899)
		holder := holdLock(t, db, lockID)

		go func() {
			time.Sleep(300 * time.Millisecond)
			discardConn(holder)
		}()

		m, err := New(db, testMigrationsFS(t), WithLockID(lockID), WithLockTimeout(30*time.Second))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("expected Run to wait for the lock, got: %v", err)
		}
	})

	t.Run("a different lock id does not contend", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		holder := holdLock(t, db, int64(112233))
		defer discardConn(holder)

		m, err := New(db, testMigrationsFS(t), WithLockID(int64(445566)))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("expected an unrelated lock id to run freely, got: %v", err)
		}
	})
}

func TestUnlock(t *testing.T) {
	db, _, closeDB := openDB(t)
	defer closeDB()

	m, err := New(db, testMigrationsFS(t), WithLockID(int64(334455)))
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed to acquire connection: %v", err)
	}
	defer discardConn(conn)

	err = m.unlock(context.Background(), conn)
	if err == nil {
		t.Fatal("expected an error releasing a lock that is not held, got nil")
	}
	if !strings.Contains(err.Error(), "not held") {
		t.Fatalf("expected a not-held error, got: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()

	if cfg.tableName != "schema_migrations" {
		t.Errorf("expected default table name schema_migrations, got %s", cfg.tableName)
	}
	if cfg.lockID != 5764249691895432819 {
		t.Errorf("expected default lock id 5764249691895432819, got %d", cfg.lockID)
	}
	if cfg.lockTimeout != 0 {
		t.Errorf("expected fail-fast by default, got a timeout of %s", cfg.lockTimeout)
	}
	if cfg.logger == nil {
		t.Error("expected a non-nil default logger")
	}
}

// unreadableFS lists its migrations but refuses to open them.
type unreadableFS struct{ fs.FS }

func (u unreadableFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".sql") {
		return nil, fs.ErrPermission
	}
	return u.FS.Open(name)
}

func TestRunFailures(t *testing.T) {
	t.Run("unusable tracking table name", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, testMigrationsFS(t), WithTableName("2bad-name"))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for an unusable table name, got nil")
		}
	})

	t.Run("tracking table missing the version column", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		if _, err := db.Exec("CREATE TABLE schema_migrations (id INT)"); err != nil {
			t.Fatalf("failed to create conflicting table: %v", err)
		}

		m, err := New(db, testMigrationsFS(t))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for a tracking table without a version column, got nil")
		}
	})

	t.Run("tracking table rejects the insert", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		if _, err := db.Exec(`
			CREATE TABLE schema_migrations (
				version TEXT PRIMARY KEY CHECK (version <> '001_first'),
				applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			)`); err != nil {
			t.Fatalf("failed to create constrained table: %v", err)
		}

		m, err := New(db, mapFS(map[string]string{"001_first.sql": "SELECT 1;"}))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error when the version cannot be recorded, got nil")
		}
	})

	t.Run("unreadable migrations directory", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, os.DirFS(filepath.Join(t.TempDir(), "missing")))
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for a missing migrations directory, got nil")
		}
	})

	t.Run("unreadable migration file", func(t *testing.T) {
		db, _, closeDB := openDB(t)
		defer closeDB()

		m, err := New(db, unreadableFS{mapFS(map[string]string{"001_first.sql": "SELECT 1;"})})
		if err != nil {
			t.Fatalf("failed to create migrator: %v", err)
		}
		if err := m.Run(context.Background()); err == nil {
			t.Fatal("expected error for an unreadable migration file, got nil")
		}
	})
}

// The message predates ErrLockNotAcquired and was the only way to detect
// contention, so callers on older versions match on it verbatim.
func TestErrLockNotAcquiredMessage(t *testing.T) {
	const want = "another migration is in progress"

	if got := ErrLockNotAcquired.Error(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}

	db, _, closeDB := openDB(t)
	defer closeDB()

	const lockID = int64(221133)
	holder := holdLock(t, db, lockID)
	defer discardConn(holder)

	m, err := New(db, testMigrationsFS(t), WithLockID(lockID))
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}

	err = m.Run(context.Background())
	if err == nil {
		t.Fatal("expected a contention error, got nil")
	}
	if err.Error() != want {
		t.Fatalf("expected Run to report %q, got %q", want, err.Error())
	}
}
