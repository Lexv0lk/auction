// Package testutil provides shared helpers for integration tests that run
// against a real PostgreSQL (make test-integration). The helpers only touch
// the database named by TEST_DATABASE_URL; the working and demo databases
// must never be pointed at them.
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/postgres"
)

const (
	connectTimeout = 10 * time.Second
	queryTimeout   = 5 * time.Second
)

// Pool returns a connection pool to the integration test database and resets
// its data. The database must already carry the current schema: the make
// test-integration targets apply the same migrations container to it before
// running the Go tests, so a schema mismatch fails the helper early.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is not set; run integration tests through make test-integration")
	}

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), connectTimeout)
	defer cancelConnect()

	pool, err := pgxpool.New(connectCtx, databaseURL)
	require.NoError(t, err, "open integration test database pool")
	t.Cleanup(pool.Close)

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), queryTimeout)
	defer cancelCheck()
	require.NoError(t, postgres.CheckSchemaVersion(checkCtx, pool), "integration test database must be migrated first")

	Reset(t, pool)

	return pool
}

// Reset clears all application tables so every test starts from a known
// state, including repeated runs (make test-integration-repeat).
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	_, err := pool.Exec(ctx, "TRUNCATE users, sessions, categories, lots, bids RESTART IDENTITY")
	require.NoError(t, err, "reset integration test data")
}
