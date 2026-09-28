// Package observability provides process-wide logging setup.
package observability

import (
	"io"
	"log/slog"
)

// NewLogger writes structured JSON events to the supplied output stream.
func NewLogger(out io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}))
}
