# Migrator

A simple and reliable database migration tool for PostgreSQL written in Go. This tool manages database schema migrations using embedded SQL files.

## Features

- Embedded SQL migration files
- Automatic migration ordering
- Migration version tracking
- Idempotent migrations (safe to run multiple times)
- Transaction-based migrations
- PostgreSQL support

## Installation

```bash
go get github.com/marcelom97/migrator
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
	"database/sql"
	"embed"
	"log"

	"github.com/marcelom97/migrator"
)

//go:embed migrations/.sql
var migrations embed.FS

func main() {
	// Connect to your database
	db, err := sql.Open("postgres", "postgresql://user:password@localhost:5432/dbname?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// Create a new migrator instance
	m := migrator.New(db, migrations)
	// Run migrations
	if err := m.Run(); err != nil {
		log.Fatal(err)
	}
}
```

## How It Works

1. Creates a `schema_migrations` table to track applied migrations
2. Reads embedded SQL files in alphabetical order
3. Executes pending migrations within transactions
4. Records successful migrations in the `schema_migrations` table

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is open-sourced under the MIT License - see the [LICENSE](LICENSE) file for details.
