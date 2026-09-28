package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
)

var errIncompleteResponse = errors.New("incomplete response")

func TestRunChecksDatabaseBeforeServing(t *testing.T) {
	cfg := config.Config{
		Database: config.Database{
			URL:      "postgres://auction:startup-secret@127.0.0.1:1/auction?sslmode=disable",
			MaxConns: 2,
			Timeout:  2 * time.Second,
		},
		ShutdownTimeout: time.Second,
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()

	select {
	case err := <-result:
		require.Error(t, err)
		assert.ErrorContains(t, err, "database")
		assert.NotContains(t, err.Error(), "startup-secret", "the password must not leak into diagnostics")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "startup did not fail on an unreachable database")
	}
	assert.Less(t, time.Since(started), 5*time.Second, "startup check must finish within the configured deadline")
}

func TestShutdown(t *testing.T) {
	for _, forced := range []bool{false, true} {
		name := "waits for active request"
		if forced {
			name = "closes on deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			addr := listener.Addr().String()
			entered, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(release) })
			server := &http.Server{
				ReadHeaderTimeout: time.Second,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					close(entered)
					<-release
					_, _ = io.WriteString(w, "completed")
				}),
			}
			server.RegisterOnShutdown(func() { close(shuttingDown) })
			t.Cleanup(func() { _ = server.Close() })
			deadline := 3 * time.Second
			if forced {
				deadline = 50 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() {
				result <- serve(ctx, server, listener, slog.New(slog.NewJSONHandler(io.Discard, nil)), deadline)
			}()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr, nil)
			require.NoError(t, err)
			response := make(chan error, 1)
			go func() {
				client := &http.Client{Timeout: 5 * time.Second}
				res, err := client.Do(request)
				if err == nil {
					var body []byte
					body, err = io.ReadAll(res.Body)
					_ = res.Body.Close()
					if err == nil && string(body) != "completed" {
						err = errIncompleteResponse
					}
				}
				response <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "request did not start")
			}
			cancel()
			select {
			case <-shuttingDown:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "shutdown did not start")
			}
			if !forced {
				select {
				case err := <-result:
					require.FailNowf(t, "early shutdown", "shutdown returned before the active request: %v", err)
				default:
				}
				release <- struct{}{}
			}
			select {
			case err := <-result:
				if forced {
					assert.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					assert.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				require.FailNow(t, "shutdown exceeded its budget")
			}
			select {
			case err := <-response:
				if !forced {
					assert.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				require.FailNow(t, "client did not finish")
			}
			rebound, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
			require.NoError(t, err, "port was not released")
			_ = rebound.Close()
		})
	}
}
