package httpapp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gorilla/csrf"

	"github.com/Lexv0lk/auction/internal/auth"
)

// sessionCookieName is the browser-side session token; the database never sees
// it raw, only its SHA-256 hash.
const sessionCookieName = "auction_session"

// markPlaintext tells the CSRF middleware that this environment is explicitly
// configured as local plain HTTP (COOKIE_SECURE=false): browsers are not
// required to supply Origin or Referer there. In HTTPS mode the strict
// same-origin checking stays enabled.
func (h *Handler) markPlaintext(next http.Handler) http.Handler {
	if h.config.CookieSecure {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), csrf.PlaintextHTTPContextKey, true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// limitLoginBody rejects oversized login submissions before the CSRF
// middleware reads and parses the form body. Requests without a
// Content-Length are still capped by the MaxBytesReader; their failure then
// surfaces through the CSRF check instead of a dedicated status.
func (h *Handler) limitLoginBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if r.ContentLength > maxLoginBodyBytes {
				h.Error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "Слишком большое тело запроса")

				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
		}

		next.ServeHTTP(w, r)
	})
}

// withUser validates the session cookie on every request and loads the
// account into the context. A missing, expired or substituted cookie simply
// leaves the request as a guest; a database failure is a technical 503, never
// a silent downgrade to guest (which would close sessions during outages) and
// never a "wrong password" style answer.
func (h *Handler) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			next.ServeHTTP(w, r)

			return
		}
		user, err := h.auth.User(r.Context(), cookie.Value)
		if errors.Is(err, auth.ErrNoSession) {
			next.ServeHTTP(w, r)

			return
		}
		if err != nil {
			h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен, попробуйте позже")

			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
	})
}

// requireUser gates a handler behind an authenticated session: guests get a
// redirect to the login page for HTML routes and 401 for JSON API routes.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)

			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Error(w, r, http.StatusUnauthorized, "unauthenticated", "Требуется вход в систему")

			return
		}

		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// requireRole additionally enforces the account role. The role always comes
// from the session loaded by withUser, so a client cannot elevate itself by
// changing form fields, JSON bodies or headers.
func (h *Handler) requireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFromContext(r.Context())
		if !ok {
			h.Error(w, r, http.StatusUnauthorized, "unauthenticated", "Требуется вход в систему")

			return
		}
		if user.Role != role {
			h.Error(w, r, http.StatusForbidden, "forbidden", "Недостаточно прав для этого действия")

			return
		}

		next.ServeHTTP(w, r)
	})
}
