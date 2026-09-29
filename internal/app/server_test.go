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

var errBackgroundFailure = errors.New("background loop failure")

// stubLoop stands in for the auction worker: it blocks until the context is
// done, fails, or panics, depending on what the test needs to observe.
type stubLoop struct {
	behavior func(ctx context.Context) error
}

func (s stubLoop) Run(ctx context.Context, _ config.Worker) error {
	return s.behavior(ctx)
}

func stubContextLoop() stubLoop {
	return stubLoop{behavior: func(ctx context.Context) error {
		<-ctx.Done()

		return nil
	}}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

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
	go func() { result <- Run(ctx, cfg, testLogger()) }()

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
				result <- runLifecycle(ctx, server, listener, testLogger(), deadline, stubContextLoop(), config.Worker{}, nil)
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

func TestFatalBackgroundErrorStopsHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	t.Cleanup(func() { _ = server.Close() })

	result := make(chan error, 1)
	go func() {
		result <- runLifecycle(ctx, server, listener, testLogger(), 3*time.Second,
			stubLoop{behavior: func(context.Context) error { return errBackgroundFailure }}, config.Worker{}, nil)
	}()

	select {
	case err := <-result:
		require.Error(t, err, "a background loop that stopped first is a fatal application error")
		assert.ErrorIs(t, err, errBackgroundFailure)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the application did not stop after the background failure")
	}

	rebound, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
	require.NoError(t, err, "the fatal background error must release the HTTP port")
	_ = rebound.Close()
}

func TestBackgroundPanicStopsHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	t.Cleanup(func() { _ = server.Close() })

	result := make(chan error, 1)
	go func() {
		result <- runLifecycle(ctx, server, listener, testLogger(), 3*time.Second,
			stubLoop{behavior: func(context.Context) error { panic("broken loop") }}, config.Worker{}, nil)
	}()

	select {
	case err := <-result:
		require.Error(t, err, "a panicking background goroutine must stop the application")
		assert.ErrorContains(t, err, "panicked")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the application did not stop after the background panic")
	}

	rebound, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
	require.NoError(t, err, "the panic must release the HTTP port")
	_ = rebound.Close()
}

func TestHTTPFailureStopsBackgroundLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close(), "the test closes the listener to break Serve")
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	t.Cleanup(func() { _ = server.Close() })

	stopped := make(chan struct{})
	loop := stubLoop{behavior: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)

		return nil
	}}

	result := make(chan error, 1)
	go func() {
		result <- runLifecycle(ctx, server, listener, testLogger(), 3*time.Second, loop, config.Worker{}, nil)
	}()

	select {
	case err := <-result:
		require.Error(t, err, "a broken HTTP listener is a fatal application error")
		assert.ErrorContains(t, err, "http server")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the application did not stop after the HTTP failure")
	}

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "the background loop must stop when HTTP fails")
	}

	rebound, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", addr)
	require.NoError(t, err, "the HTTP failure must release the port")
	_ = rebound.Close()
}

func TestShutdownMarksProcessNotReady(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	t.Cleanup(func() { _ = server.Close() })

	drainingCalls := 0
	result := make(chan error, 1)
	go func() {
		result <- runLifecycle(ctx, server, listener, testLogger(), 3*time.Second,
			stubContextLoop(), config.Worker{}, func() { drainingCalls++ })
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-result:
		require.NoError(t, err)
		assert.Equal(t, 1, drainingCalls, "the readiness probe flips exactly once, when the shutdown starts")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the application did not stop after the cancellation")
	}
}
