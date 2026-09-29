package httpapp

import (
	"net/http"

	"github.com/gorilla/csrf"
)

// csrf wraps the mux with gorilla/csrf: every mutating request must carry a
// masked token tied to the signed _gorilla_csrf cookie (HTML forms embed the
// token as a hidden field, JavaScript sends it in the X-CSRF-Token header).
// The token lives in the signed cookie alone, so the check survives restarts
// and replica switches as long as all replicas derive their key from the same
// CSRF_SECRET. SameSite=Lax on both cookies is defense in depth, not a
// replacement for the token check.
func (h *Handler) csrf(next http.Handler) http.Handler {
	return csrf.Protect(
		h.config.CSRFKey,
		csrf.Secure(h.config.CookieSecure),
		csrf.SameSite(csrf.SameSiteLaxMode),
		csrf.ErrorHandler(http.HandlerFunc(h.csrfRejected)),
	)(next)
}

func (h *Handler) csrfRejected(w http.ResponseWriter, r *http.Request) {
	if reason := csrf.FailureReason(r); reason != nil {
		h.logger.Warn("csrf check failed", "reason", reason)
	}
	h.Error(w, r, http.StatusForbidden, "csrf_invalid", "Проверка CSRF не пройдена; обновите страницу и повторите действие")
}
