// Package postgres owns the shared PostgreSQL connection pool and the schema
// version contract. The server never applies migrations itself; they are
// applied by the separate migration container (make migrate).
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ExpectedSchemaVersion is the schema version this binary supports. Bump it
// together with the next migration in ../migrations; the version table of the
// migration tool must match exactly before the server serves HTTP.
const ExpectedSchemaVersion int64 = 4

const schemaVersionQuery = "SELECT version, dirty FROM schema_migrations"

// pgErrCodeUndefinedTable marks a missing schema_migrations table, which means
// no migration was ever applied to this database.
const pgErrCodeUndefinedTable = "42P01"

var (
	// ErrSchemaNotMigrated means the migration tool has not initialized the database.
	ErrSchemaNotMigrated = errors.New("database schema is not initialized; apply migrations with make migrate")
	// ErrSchemaDirty means the previous migration run did not finish cleanly.
	ErrSchemaDirty = errors.New("database schema migration did not finish; resolve it and rerun make migrate")
	// ErrSchemaNotSupported means the applied schema version differs from the one this binary expects.
	ErrSchemaNotSupported = errors.New("database schema version is not supported; apply migrations with make migrate")
)

// SchemaQuerier is the subset of the connection pool used by the version check.
type SchemaQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckSchemaVersion verifies that the database carries exactly the schema
// version this binary supports and that no migration run is stuck mid-way.
// It never changes the schema and is safe to reuse for readiness checks.
func CheckSchemaVersion(ctx context.Context, db SchemaQuerier) error {
	var version int64
	var dirty bool

	err := db.QueryRow(ctx, schemaVersionQuery).Scan(&version, &dirty)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSchemaNotMigrated
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeUndefinedTable {
			return ErrSchemaNotMigrated
		}

		return fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return ErrSchemaDirty
	}
	if version != ExpectedSchemaVersion {
		return fmt.Errorf("%w (found version %d, supported version %d)", ErrSchemaNotSupported, version, ExpectedSchemaVersion)
	}

	return nil
}
