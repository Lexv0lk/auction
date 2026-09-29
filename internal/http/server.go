// Package httpapp contains the HTTP entry point and shared error handling.
package httpapp

import (
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/observability"
	"github.com/Lexv0lk/auction/web"
)

// Service route labels: the request log and the HTTP metrics name these
// routes by their fixed path, not by the raw request URL.
const (
	routeLivez   = "/livez"
	routeReadyz  = "/readyz"
	routeMetrics = "/metrics"
)

// Handler holds the shared HTTP presentation dependencies.
type Handler struct {
	logger     *slog.Logger
	auth       Authenticator
	categories Categories
	lots       Lots
	metrics    Metrics
	config     Config
	pages      map[string]*template.Template
	static     http.Handler
}

// NewHandler loads embedded assets and constructs the HTTP router. A nil
// metrics dependency falls back to a fresh instrument set without a pool
// snapshot, so the exporter always answers with a valid exposition.
func NewHandler(logger *slog.Logger, authenticator Authenticator, categories Categories, lots Lots, metrics Metrics, config Config) (http.Handler, error) {
	pages, err := parsePageTemplates()
	if err != nil {
		return nil, fmt.Errorf("load page templates: %w", err)
	}

	staticFiles, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}

	if metrics == nil {
		metrics = observability.NewMetrics(nil)
	}
	h := &Handler{logger: logger, auth: authenticator, categories: categories, lots: lots, metrics: metrics, config: config, pages: pages, static: http.FileServer(http.FS(staticFiles))}
	mux := http.NewServeMux()

	// Every application route is wrapped with its template before any
	// middleware runs, so the request log and the HTTP metrics carry the
	// route pattern ("/lots/{id}") instead of the concrete request path.
	mux.Handle("GET /static/", h.route("/static/", http.StripPrefix("/static/", h.static)))
	mux.Handle("GET /login", h.route("/login", http.HandlerFunc(h.loginPage)))
	mux.Handle("POST /login", h.route("/login", http.HandlerFunc(h.loginSubmit)))
	mux.Handle("POST /logout", h.route("/logout", h.requireUser(http.HandlerFunc(h.logoutSubmit))))
	mux.Handle("GET /{$}", h.route("/", h.requireUser(http.HandlerFunc(h.homePage))))

	// The reference data is an administrative area: every route, read or
	// write, checks the session and then the admin role, so a direct handler
	// call is guarded exactly like a routed request. The lot routes follow
	// the same rule.
	adminOnly := func(next http.Handler) http.Handler {
		return h.requireUser(h.requireRole(auth.RoleAdmin, next))
	}
	mux.Handle("GET /admin/categories", h.route("/admin/categories", adminOnly(http.HandlerFunc(h.categoriesPage))))
	mux.Handle("POST /admin/categories", h.route("/admin/categories", adminOnly(http.HandlerFunc(h.categoryCreate))))
	mux.Handle("GET /admin/categories/{id}/edit", h.route("/admin/categories/{id}/edit", adminOnly(http.HandlerFunc(h.categoryEditPage))))
	mux.Handle("POST /admin/categories/{id}", h.route("/admin/categories/{id}", adminOnly(http.HandlerFunc(h.categoryRename))))
	mux.Handle("POST /admin/categories/{id}/delete", h.route("/admin/categories/{id}/delete", adminOnly(http.HandlerFunc(h.categoryDelete))))

	mux.Handle("GET /admin/lots", h.route("/admin/lots", adminOnly(http.HandlerFunc(h.lotsPage))))
	mux.Handle("GET /admin/lots/new", h.route("/admin/lots/new", adminOnly(http.HandlerFunc(h.lotNewPage))))
	mux.Handle("POST /admin/lots", h.route("/admin/lots", adminOnly(http.HandlerFunc(h.lotCreate))))
	mux.Handle("GET /admin/lots/{id}/edit", h.route("/admin/lots/{id}/edit", adminOnly(http.HandlerFunc(h.lotEditPage))))
	mux.Handle("POST /admin/lots/{id}", h.route("/admin/lots/{id}", adminOnly(http.HandlerFunc(h.lotUpdate))))
	mux.Handle("POST /admin/lots/{id}/delete", h.route("/admin/lots/{id}/delete", adminOnly(http.HandlerFunc(h.lotDelete))))
	mux.Handle("POST /admin/lots/{id}/publish", h.route("/admin/lots/{id}/publish", adminOnly(http.HandlerFunc(h.lotPublish))))

	// The participant area is open to every authenticated role: the catalog
	// and the lot page read only published lots, so a draft is invisible here
	// by construction (the service answers it as a missing lot). The API
	// routes answer guests with 401 JSON, the pages with a login redirect.
	// Bidding is the one participant-only action: the administrator is
	// refused before any service call.
	mux.Handle("GET /lots", h.route("/lots", h.requireUser(http.HandlerFunc(h.catalogPage))))
	mux.Handle("GET /lots/{id}", h.route("/lots/{id}", h.requireUser(http.HandlerFunc(h.lotPublicPage))))
	mux.Handle("GET /api/lots/{id}", h.route("/api/lots/{id}", h.requireUser(http.HandlerFunc(h.lotStateAPI))))
	participantOnly := func(next http.Handler) http.Handler {
		return h.requireUser(h.requireRole(auth.RoleParticipant, next))
	}
	mux.Handle("POST /lots/{id}/bids", h.route("/lots/{id}/bids", participantOnly(http.HandlerFunc(h.bidFormSubmit))))
	mux.Handle("POST /api/lots/{id}/bids", h.route("/api/lots/{id}/bids", participantOnly(http.HandlerFunc(h.bidAPISubmit))))

	// The catch-all answers everything the mux did not route; its fixed
	// "unmatched" label keeps unknown paths out of the metric series.
	mux.Handle("/", h.route("unmatched", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Error(w, r, http.StatusNotFound, "not_found", "Страница не найдена")
	})))

	// Middleware order, outermost first: request ID and panic recovery, the
	// service routes (probes and the metrics exporter: no cookies, no session
	// work), the body limits, the plaintext marker for local HTTP, CSRF,
	// session loading. The request ID is therefore available to the CSRF and
	// session error answers, and the panic log. The body limits run before
	// CSRF, which parses form bodies itself.
	wrappedHandler := h.enrichWithID(h.exceptServiceRoutes(h.limitLoginBody(h.limitBidBody(h.markPlaintext(h.csrf(h.withUser(mux)))))))

	return wrappedHandler, nil
}

// route marks every answer of the wrapped handler with the route template
// label; the request log and the HTTP metrics read it from the request state.
func (h *Handler) route(template string, next http.Handler) http.Handler {
	label := routeLabel(template)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeStateFrom(r.Context()).setRoute(label)
		next.ServeHTTP(w, r)
	})
}

// routeLabel strips the method prefix from a ServeMux pattern and normalizes
// the home page pattern, leaving the bare route template.
func routeLabel(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	if pattern == "/{$}" {
		return "/"
	}

	return pattern
}

// exceptServiceRoutes answers the health probes and the metrics exporter
// before the CSRF and session middleware, so they run without cookies and
// never touch the session store. The probes are public by design: the
// environment restricts their access through the address HTTP_ADDR binds to,
// and the METRICS_ENABLED switch keeps the exporter off where it is not
// wanted.
func (h *Handler) exceptServiceRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := routeStateFrom(r.Context())
		switch r.URL.Path {
		case "/livez":
			state.setRoute(routeLivez)
			h.livez(w, r)
		case "/readyz":
			state.setRoute(routeReadyz)
			h.readyz(w, r)
		case "/metrics":
			state.setRoute(routeMetrics)
			h.metricsEndpoint(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyz answers 200 only while the process can serve traffic: a draining
// process (the shutdown has started) and a process whose database check fails
// both answer 503 without any diagnosis in the body — the details stay in the
// log, tied to the request ID.
func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	const notReady = "not ready\n"
	if h.config.Draining != nil && h.config.Draining() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(notReady))

		return
	}
	if h.config.Readiness == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))

		return
	}
	if err := h.config.Readiness(r.Context()); err != nil {
		h.logger.Warn("readiness check failed", "operation", "readyz", "outcome", "error", "error_code", "not_ready", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(notReady))

		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// metricsEndpoint serves the Prometheus exporter when the environment enabled
// it; otherwise the route answers like any unknown page.
func (h *Handler) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	if !h.config.MetricsEnabled {
		h.Error(w, r, http.StatusNotFound, "not_found", "Страница не найдена")

		return
	}
	h.metrics.Handler().ServeHTTP(w, r)
}
