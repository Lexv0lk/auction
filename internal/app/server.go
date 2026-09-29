// Package app assembles the application and manages its lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/config"
	httpapp "github.com/Lexv0lk/auction/internal/http"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/postgres"
	"github.com/Lexv0lk/auction/internal/worker"
)

// errBackgroundStopped names the unexpected clean stop of the background
// loop: it must only stop through the shared application context.
var errBackgroundStopped = errors.New("background worker stopped unexpectedly")

// errWorkerPanicked wraps the recovered value of a panicking background
// goroutine.
var errWorkerPanicked = errors.New("background worker panicked")

// errWorkerShutdownTimeout names a background loop that outlived the shared
// shutdown budget.
var errWorkerShutdownTimeout = errors.New("background worker did not stop within the shutdown budget")

// backgroundLoop is the long-running work beside HTTP: the auction worker in
// production, a controlled stub in the tests.
type backgroundLoop interface {
	Run(ctx context.Context, cfg config.Worker) error
}

// Run assembles the application and drives it until the context is
// cancelled. HTTP and the background auction loop live in this one process:
// they share the pool and the shutdown budget, so replicas scale together
// and no separate worker binary or profile exists.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := postgres.Connect(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The server refuses to serve before the database carries exactly the
	// schema version this release supports; it never applies migrations itself.
	schemaCtx, cancel := context.WithTimeout(ctx, cfg.Database.Timeout)
	defer cancel()
	if err := postgres.CheckSchemaVersion(schemaCtx, pool); err != nil {
		return err
	}

	handler, err := httpapp.NewHandler(logger, auth.NewService(pool), category.NewService(pool), lot.NewService(pool), httpapp.Config{
		SessionTTL:   cfg.Session.TTL,
		CookieSecure: cfg.Session.CookieSecure,
		CSRFKey:      httpapp.NewCSRFKey(cfg.Session.CSRFSecret),
	})
	if err != nil {
		return err
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	auctionWorker := worker.New(pool, logger)

	return runLifecycle(ctx, server, listener, logger, cfg.ShutdownTimeout, auctionWorker, cfg.Worker)
}

// runLifecycle drives HTTP and the background loop inside one shutdown
// budget. The loop watches the same context as the process, so one signal
// stops taking bids and selecting lots at once; the pool closes only after
// both components have stopped, and a transaction caught mid-flight rolls
// back on its own short context, leaving its lot to the next replica.
func runLifecycle(ctx context.Context, server *http.Server, listener net.Listener, logger *slog.Logger, shutdownTimeout time.Duration, background backgroundLoop, workerCfg config.Worker) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	logger.Info("http server started", "addr", listener.Addr().String())

	backgroundDone := make(chan error, 1)
	go func() {
		// An unexpected end of the background goroutine is a fatal
		// application error, and a panic must not pass silently.
		defer func() {
			if r := recover(); r != nil {
				backgroundDone <- fmt.Errorf("%w: %v", errWorkerPanicked, r)
			}
		}()
		backgroundDone <- background.Run(runCtx, workerCfg)
	}()

	var runErr error
	serveDone, backgroundDoneSeen := false, false
	select {
	case err := <-serveResult:
		serveDone = true
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("http server: %w", err)
		}
	case err := <-backgroundDone:
		backgroundDoneSeen = true
		if ctx.Err() == nil {
			// The loop stopped while the application is still running: bring
			// HTTP down with it and report the reason.
			reason := backgroundStopReason(err)
			logger.Error("background worker stopped unexpectedly", "error", reason.Error())
			runErr = fmt.Errorf("background worker: %w", reason)
		}
	case <-ctx.Done():
		logger.Info("http server shutting down")
	}

	// Nothing new is served or selected from here: the background loop stops
	// through its context (a no-op when it already stopped).
	cancelRun()

	// One budget covers the HTTP drain and the background loop together.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		if runErr == nil {
			runErr = fmt.Errorf("http shutdown: %w", err)
		}
	}

	if !backgroundDoneSeen {
		select {
		case err := <-backgroundDone:
			if runErr == nil && err != nil {
				runErr = fmt.Errorf("background worker: %w", err)
			}
		case <-shutdownCtx.Done():
			if runErr == nil {
				runErr = fmt.Errorf("%w (%s)", errWorkerShutdownTimeout, shutdownTimeout)
			}
		}
	}

	if !serveDone {
		if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) && runErr == nil {
			runErr = fmt.Errorf("http server: %w", err)
		}
	}

	if runErr == nil {
		logger.Info("http server stopped")
	}

	return runErr
}

// backgroundStopReason normalizes the result of a background loop that
// stopped while the application was still running: even a nil error is an
// unexpected stop there.
func backgroundStopReason(err error) error {
	if err == nil {
		return errBackgroundStopped
	}

	return err
}
