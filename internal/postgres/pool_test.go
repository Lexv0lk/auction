package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/postgres"
)

func TestConnectReportsUnreachableDatabaseWithoutPassword(t *testing.T) {
	database := config.Database{
		URL:      "postgres://auction:super-secret-password@127.0.0.1:1/auction?sslmode=disable",
		MaxConns: 2,
		Timeout:  2 * time.Second,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	started := time.Now()
	pool, err := postgres.Connect(ctx, database)
	if pool != nil {
		pool.Close()
	}
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.ErrorContains(t, err, "database")
	assert.NotContains(t, err.Error(), "super-secret-password", "the password must not leak into diagnostics")
	assert.Less(t, elapsed, 5*time.Second, "connection attempt must finish within the configured deadline")
}
