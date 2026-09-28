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

	"github.com/Lexv0lk/auction/internal/config"
	httpapp "github.com/Lexv0lk/auction/internal/http"
	"github.com/Lexv0lk/auction/internal/postgres"
)

// Run serves HTTP until cancellation. Step 11 will add the background auction
// loop to this same lifecycle, using the same dependencies and shutdown budget.
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

	handler, err := httpapp.NewHandler(logger)
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

	return serve(ctx, server, listener, logger, cfg.ShutdownTimeout)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, logger *slog.Logger, shutdownTimeout time.Duration) error {
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	logger.Info("http server started", "addr", listener.Addr().String())

	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}

		return nil
	case <-ctx.Done():
		logger.Info("http server shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()

		return fmt.Errorf("http shutdown: %w", err)
	}

	if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	logger.Info("http server stopped")

	return nil
}
