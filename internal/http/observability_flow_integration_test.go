//go:build integration

package httpapp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/observability"
	"github.com/Lexv0lk/auction/internal/postgres"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// realReadiness is the same readiness the application wires: a pool ping
// plus the schema version contract.
func realReadiness(pool *pgxpool.Pool, timeout time.Duration) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := pool.Ping(probeCtx); err != nil {
			return err
		}

		return postgres.CheckSchemaVersion(probeCtx, pool)
	}
}

func TestProbesAndMetricsAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)

	metrics := observability.NewMetrics(func() observability.PoolSnapshot {
		stats := pool.Stat()

		return observability.PoolSnapshot{
			TotalConns:    stats.TotalConns(),
			AcquiredConns: stats.AcquiredConns(),
			IdleConns:     stats.IdleConns(),
			MaxConns:      stats.MaxConns(),
		}
	})
	config := testConfig()
	config.MetricsEnabled = true
	config.Readiness = realReadiness(pool, 5*time.Second)
	handler, err := NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)),
		auth.NewService(pool), category.NewService(pool), lot.NewService(pool), metrics, config)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(server.URL + "/livez") //nolint:noctx // a fixed local target with a timeout on the client
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok\n", string(body))

	resp, err = client.Get(server.URL + "/readyz") //nolint:noctx // a fixed local target with a timeout on the client
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a migrated database answers ready")

	resp, err = client.Get(server.URL + "/metrics") //nolint:noctx // a fixed local target with a timeout on the client
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	exposition := string(body)
	for _, fragment := range []string{
		"go_goroutines",
		"process_resident_memory_bytes",
		"auction_http_requests_total",
		"auction_worker_last_success_timestamp_seconds",
		"auction_db_pool_total_conns",
		"auction_db_pool_max_conns",
	} {
		assert.Contains(t, exposition, fragment, "the exporter must carry %s", fragment)
	}
}

func TestReadyzAnswersNotReadyOnUnreachableDatabase(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	deadPool, err := pgxpool.New(context.Background(),
		"postgres://auction:probe@127.0.0.1:1/auction?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(deadPool.Close)

	config := testConfig()
	config.MetricsEnabled = true
	config.Readiness = realReadiness(deadPool, 2*time.Second)
	handler, err := NewHandler(slog.New(slog.NewJSONHandler(io.Discard, nil)),
		&fakeAuthenticator{}, &fakeCategories{}, &fakeLots{}, metrics, config)
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	started := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(server.URL + "/readyz") //nolint:noctx // a fixed local target
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.True(t, strings.HasPrefix(string(body), "not ready"), "the body names no technical cause")
	assert.Less(t, time.Since(started), 3*time.Second, "the refusal comes fast, not after the whole budget")

	// The liveness probe stays independent: the process itself is alive.
	resp, err = (&http.Client{Timeout: 5 * time.Second}).Get(server.URL + "/livez") //nolint:noctx // a fixed local target
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
