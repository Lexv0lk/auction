package httpapp

import (
	"errors"
	"net/http"

	"github.com/Lexv0lk/auction/internal/auth"
)

// maxLoginBodyBytes bounds the login form submission; oversized requests are
// rejected before any parsing work.
const maxLoginBodyBytes = 4096

// newSessionCookie returns the cookie carrying the raw session token. The
// token is issued fresh on every login, never logged and stored only as a
// SHA-256 hash in the database. The Secure flag follows COOKIE_SECURE: it may
// be disabled only by explicitly configuring a local plain-HTTP environment.
func newSessionCookie(token string, config Config) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // G124 // Secure follows the COOKIE_SECURE setting; disabled only for local HTTP
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(config.SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   config.CookieSecure,
	}
}

// clearedSessionCookie instructs the browser to drop the session cookie.
func clearedSessionCookie(config Config) *http.Cookie {
	cookie := newSessionCookie("", config) //nolint:gosec // G124 // Secure follows the COOKIE_SECURE setting; disabled only for local HTTP
	cookie.MaxAge = -1

	return cookie
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	data := h.newPageData(r)
	data.Title = "Вход"
	h.renderPage(w, r, http.StatusOK, "login.html", loginData{pageData: data})
}

func (h *Handler) loginSubmit(w http.ResponseWriter, r *http.Request) {
	// The body is already capped by limitLoginBody; this ParseForm only
	// reports parsing problems for requests whose body the CSRF middleware
	// has not consumed (token supplied via header instead of form field).
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.Error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "Слишком большое тело запроса")
		} else {
			h.Error(w, r, http.StatusBadRequest, "invalid_request", "Некорректный запрос")
		}

		return
	}

	login := r.PostFormValue("login")
	passwordValue := r.PostFormValue("password")
	token, _, err := h.auth.Login(r.Context(), login, passwordValue, h.config.SessionTTL)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		data := h.newPageData(r)
		data.Title = "Вход"
		data.Error = "Неверный логин или пароль"
		h.renderPage(w, r, http.StatusUnauthorized, "login.html", loginData{pageData: data, Login: auth.NormalizeLogin(login)})

		return
	}
	if err != nil {
		// A database failure must not look like rejected credentials and must
		// not leak its technical details into the response.
		h.logger.Error("login failed", "error", err)
		h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен, попробуйте позже")

		return
	}

	http.SetCookie(w, newSessionCookie(token, h.config))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) logoutSubmit(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := h.auth.Logout(r.Context(), cookie.Value); err != nil {
			h.logger.Error("logout failed", "error", err)
			h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен, попробуйте позже")

			return
		}
	}

	http.SetCookie(w, clearedSessionCookie(h.config))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) homePage(w http.ResponseWriter, r *http.Request) {
	data := h.newPageData(r)
	data.Title = "Учебный аукцион"
	h.renderPage(w, r, http.StatusOK, "home.html", homeData{pageData: data})
}
