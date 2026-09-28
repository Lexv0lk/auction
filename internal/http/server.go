// Package httpapp contains the HTTP entry point and shared error handling.
package httpapp

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/Lexv0lk/auction/web"
)

// Handler holds the shared HTTP presentation dependencies.
type Handler struct {
	logger        *slog.Logger
	errorTemplate *template.Template
	static        http.Handler
}

// NewHandler loads embedded assets and constructs the HTTP router.
func NewHandler(logger *slog.Logger) (http.Handler, error) {
	t, err := template.ParseFS(web.Files, "templates/error.gohtml")
	if err != nil {
		return nil, fmt.Errorf("load error template: %w", err)
	}

	staticFiles, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}

	h := &Handler{logger: logger, errorTemplate: t, static: http.FileServer(http.FS(staticFiles))}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /livez", h.livez)
	mux.Handle("GET /static/", http.StripPrefix("/static/", h.static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.Error(w, r, http.StatusNotFound, "not_found", "Страница не найдена")
	})

	wrappedHandler := h.enrichWithID(
		h.recoverPanic(mux),
	)

	return wrappedHandler, nil
}

func (h *Handler) livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
