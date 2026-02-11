# Migrator

A simple and reliable database migration tool for PostgreSQL written in Go. This tool manages database schema migrations using embedded SQL files.

## Features

- Embedded SQL migration files
- Automatic migration ordering
- Migration version tracking
- Idempotent migrations (safe to run multiple times)
- Transaction-based migrations
- Concurrent safety via PostgreSQL advisory locks
- Configurable table name, lock ID, and structured logging

## Installation

```bash
go get github.com/marcelom97/migrator/v2
```

## Usage

### Creating Migration Files

Create your SQL migration files in a directory (e.g., `migrations/`). Files should be named with a numeric prefix for ordering:

```bash
migrations/
├── 001_create_users_table.sql
├── 002_add_email_to_users.sql
├── 003_create_posts_table.sql
```

### Running Migrations

```go
package main

import (
	"context"
	"database/sql"
	"embed"
	"io/fs"
	"log"
	"log/slog"
	"os"

	"github.com/marcelom97/migrator/v2"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func main() {
	db, err := sql.Open("postgres", "postgresql://user:password@localhost:5432/dbname?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		log.Fatal(err)
	}

	m, err := migrator.New(db, migrations,
		migrator.WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil))),
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := m.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

### Configuration Options

```go
// Custom migration tracking table name (default: "schema_migrations")
migrator.WithTableName("my_migrations")

// Custom advisory lock ID (default: 5764249691895432819)
migrator.WithLockID(42)

// Custom structured logger (default: no-op)
migrator.WithLogger(slog.New(slog.NewTextHandler(os.Stdout, nil)))
```

## How It Works

1. Acquires a dedicated database connection for advisory lock management
2. Acquires a PostgreSQL advisory lock to prevent concurrent migrations
3. Creates a migration tracking table (configurable name)
4. Wraps all operations in a transaction for atomicity
5. Reads embedded SQL files in alphabetical order
6. Executes pending migrations within the transaction
7. Records successful migrations in the tracking table
8. Releases the advisory lock

## Concurrent Safety

The migrator is designed to be safe in distributed environments where multiple instances might try to run migrations simultaneously:

- Uses PostgreSQL advisory locks on a dedicated connection to ensure only one instance can run migrations at a time
- Other instances will receive a "another migration is in progress" error
- All database operations are wrapped in a transaction

## Design Decisions

### Forward-Only Migrations

This library intentionally does not support rollback/down migrations. Forward-only migration is a deliberate design choice:

- Rollback migrations are rarely used in production and often untested
- Fixing a bad migration by applying a new forward migration is safer and more predictable
- This keeps the library simple and focused

If you need rollback support, consider [golang-migrate](https://github.com/golang-migrate/migrate) or [goose](https://github.com/pressly/goose).

### Single Transaction

All pending migrations are applied within a single database transaction:

- Either all pending migrations succeed or none are applied (atomicity)
- DDL statements that cannot run inside a transaction (e.g., `CREATE INDEX CONCURRENTLY`) are not supported
- For most schema migrations, this is the safest approach

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is open-sourced under the MIT License - see the [LICENSE](LICENSE) file for details.
