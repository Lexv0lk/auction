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
	logger *slog.Logger
	auth   Authenticator
	config Config
	pages  map[string]*template.Template
	static http.Handler
}

// NewHandler loads embedded assets and constructs the HTTP router.
func NewHandler(logger *slog.Logger, authenticator Authenticator, config Config) (http.Handler, error) {
	pages, err := parsePageTemplates()
	if err != nil {
		return nil, fmt.Errorf("load page templates: %w", err)
	}

	staticFiles, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}

	h := &Handler{logger: logger, auth: authenticator, config: config, pages: pages, static: http.FileServer(http.FS(staticFiles))}
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", h.static))
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.loginSubmit)
	mux.Handle("POST /logout", h.requireUser(http.HandlerFunc(h.logoutSubmit)))
	mux.Handle("GET /{$}", h.requireUser(http.HandlerFunc(h.homePage)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.Error(w, r, http.StatusNotFound, "not_found", "Страница не найдена")
	})

	// Middleware order, outermost first: request ID and panic recovery, the
	// liveness probe (no cookies, no session work), the login body limit,
	// the plaintext marker for local HTTP, CSRF, session loading. The request
	// ID is therefore available to the CSRF and session error answers, and
	// the panic log.
	wrappedHandler := h.enrichWithID(h.exceptLivez(h.limitLoginBody(h.markPlaintext(h.csrf(h.withUser(mux))))))

	return wrappedHandler, nil
}

// exceptLivez serves the liveness probe before the CSRF and session
// middleware, so health checks get no cookies and never touch the database.
func (h *Handler) exceptLivez(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" {
			h.livez(w, r)

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *Handler) livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
