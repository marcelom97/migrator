package migrator

import (
	"io"
	"log/slog"
)

type config struct {
	tableName string
	lockID    int64
	logger    *slog.Logger
}

func defaultConfig() config {
	return config{
		tableName: "schema_migrations",
		lockID:    5764249691895432819, // FNV-1a hash of "github.com/marcelom97/migrator"
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Option configures the Migrator.
type Option func(*config)

// WithTableName sets the name of the migrations tracking table.
// Default: "schema_migrations".
func WithTableName(name string) Option {
	return func(c *config) {
		c.tableName = name
	}
}

// WithLockID sets the PostgreSQL advisory lock ID.
// Default: 5764249691895432819.
func WithLockID(id int64) Option {
	return func(c *config) {
		c.lockID = id
	}
}

// WithLogger sets the structured logger for migration progress.
// Default: a no-op logger.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}
