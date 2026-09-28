package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lexv0lk/auction/internal/config"
)

// errDatabaseMaxConnsTooLarge guards the pgxpool int32 limit for the pool size.
var errDatabaseMaxConnsTooLarge = errors.New("DB_MAX_CONNS exceeds the supported pool size")

// Connect opens the shared connection pool and verifies that the database is
// reachable. The caller owns the pool and must close it after every user of
// the pool, including the future background worker, has stopped.
func Connect(ctx context.Context, db config.Database) (*pgxpool.Pool, error) {
	if db.MaxConns > math.MaxInt32 {
		return nil, errDatabaseMaxConnsTooLarge
	}

	poolConfig, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	// MaxConns is bounded by the errDatabaseMaxConnsTooLarge check above.
	poolConfig.MaxConns = int32(db.MaxConns) //nolint:gosec // G115
	poolConfig.ConnConfig.ConnectTimeout = db.Timeout

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, db.Timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("connect database: %w", err)
	}

	return pool, nil
}
