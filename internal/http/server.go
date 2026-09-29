// Package httpapp contains the HTTP entry point and shared error handling.
package httpapp

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/web"
)

// Handler holds the shared HTTP presentation dependencies.
type Handler struct {
	logger     *slog.Logger
	auth       Authenticator
	categories Categories
	lots       Lots
	config     Config
	pages      map[string]*template.Template
	static     http.Handler
}

// NewHandler loads embedded assets and constructs the HTTP router.
func NewHandler(logger *slog.Logger, authenticator Authenticator, categories Categories, lots Lots, config Config) (http.Handler, error) {
	pages, err := parsePageTemplates()
	if err != nil {
		return nil, fmt.Errorf("load page templates: %w", err)
	}

	staticFiles, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}

	h := &Handler{logger: logger, auth: authenticator, categories: categories, lots: lots, config: config, pages: pages, static: http.FileServer(http.FS(staticFiles))}
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", h.static))
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.loginSubmit)
	mux.Handle("POST /logout", h.requireUser(http.HandlerFunc(h.logoutSubmit)))
	mux.Handle("GET /{$}", h.requireUser(http.HandlerFunc(h.homePage)))

	// The reference data is an administrative area: every route, read or
	// write, checks the session and then the admin role, so a direct handler
	// call is guarded exactly like a routed request. The lot routes follow
	// the same rule.
	adminOnly := func(next http.Handler) http.Handler {
		return h.requireUser(h.requireRole(auth.RoleAdmin, next))
	}
	mux.Handle("GET /admin/categories", adminOnly(http.HandlerFunc(h.categoriesPage)))
	mux.Handle("POST /admin/categories", adminOnly(http.HandlerFunc(h.categoryCreate)))
	mux.Handle("GET /admin/categories/{id}/edit", adminOnly(http.HandlerFunc(h.categoryEditPage)))
	mux.Handle("POST /admin/categories/{id}", adminOnly(http.HandlerFunc(h.categoryRename)))
	mux.Handle("POST /admin/categories/{id}/delete", adminOnly(http.HandlerFunc(h.categoryDelete)))

	mux.Handle("GET /admin/lots", adminOnly(http.HandlerFunc(h.lotsPage)))
	mux.Handle("GET /admin/lots/new", adminOnly(http.HandlerFunc(h.lotNewPage)))
	mux.Handle("POST /admin/lots", adminOnly(http.HandlerFunc(h.lotCreate)))
	mux.Handle("GET /admin/lots/{id}/edit", adminOnly(http.HandlerFunc(h.lotEditPage)))
	mux.Handle("POST /admin/lots/{id}", adminOnly(http.HandlerFunc(h.lotUpdate)))
	mux.Handle("POST /admin/lots/{id}/delete", adminOnly(http.HandlerFunc(h.lotDelete)))
	mux.Handle("POST /admin/lots/{id}/publish", adminOnly(http.HandlerFunc(h.lotPublish)))

	// The participant area is open to every authenticated role: the catalog
	// and the lot page read only published lots, so a draft is invisible here
	// by construction (the service answers it as a missing lot). The API
	// routes answer guests with 401 JSON, the pages with a login redirect.
	// Bidding is the one participant-only action: the administrator is
	// refused before any service call.
	mux.Handle("GET /lots", h.requireUser(http.HandlerFunc(h.catalogPage)))
	mux.Handle("GET /lots/{id}", h.requireUser(http.HandlerFunc(h.lotPublicPage)))
	mux.Handle("GET /api/lots/{id}", h.requireUser(http.HandlerFunc(h.lotStateAPI)))
	participantOnly := func(next http.Handler) http.Handler {
		return h.requireUser(h.requireRole(auth.RoleParticipant, next))
	}
	mux.Handle("POST /lots/{id}/bids", participantOnly(http.HandlerFunc(h.bidFormSubmit)))
	mux.Handle("POST /api/lots/{id}/bids", participantOnly(http.HandlerFunc(h.bidAPISubmit)))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.Error(w, r, http.StatusNotFound, "not_found", "Страница не найдена")
	})

	// Middleware order, outermost first: request ID and panic recovery, the
	// liveness probe (no cookies, no session work), the body limits, the
	// plaintext marker for local HTTP, CSRF, session loading. The request ID
	// is therefore available to the CSRF and session error answers, and the
	// panic log. The body limits run before CSRF, which parses form bodies
	// itself.
	wrappedHandler := h.enrichWithID(h.exceptLivez(h.limitLoginBody(h.limitBidBody(h.markPlaintext(h.csrf(h.withUser(mux)))))))

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
