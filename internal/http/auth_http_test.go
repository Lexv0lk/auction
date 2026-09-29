package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
)

var (
	errDatabaseDown = errors.New("database down")
	errLoginReached = errors.New("login must not be reached without CSRF")
)

func sessionCookie(resp *httpResponse) *http.Cookie {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "auction_session" {
			return cookie
		}
	}

	return nil
}

// setSessionCookie simulates the browser holding a session cookie from a
// previous successful login.
func setSessionCookie(t *testing.T, client *http.Client, serverURL, value string) {
	t.Helper()

	cookieURL, err := url.Parse(serverURL)
	require.NoError(t, err)
	//nolint:gosec // G124 // simulates a cookie a previous login set; attributes come from the server, not the jar
	client.Jar.SetCookies(cookieURL, []*http.Cookie{{Name: "auction_session", Value: value}})
}

func TestGuestGetsRedirectedToLogin(t *testing.T) {
	server, client := newTestServer(t, &fakeAuthenticator{}, testConfig())

	resp := getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
}

func TestRequireUserGuardsApiAndHtml(t *testing.T) {
	handler := &Handler{pages: mustParsePages(t), logger: discardLogger()}
	protected := handler.requireUser(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a guest must never reach the wrapped handler")
	}))

	recorder := httptest.NewRecorder()
	protected.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/lots", nil))
	assert.Equal(t, http.StatusSeeOther, recorder.Code)
	assert.Equal(t, "/login", recorder.Header().Get("Location"))

	recorder = httptest.NewRecorder()
	protected.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/lots/1", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.Equal(t, "unauthenticated", payload.Error.Code)
}

func TestLoginFlowIssuesFreshCookie(t *testing.T) {
	var capturedLogin, capturedPassword string
	var capturedTTL time.Duration
	authenticator := &fakeAuthenticator{
		loginFunc: func(_ context.Context, login, password string, ttl time.Duration) (string, auth.User, error) {
			capturedLogin, capturedPassword, capturedTTL = login, password, ttl

			return strings.Repeat("a", 64), auth.User{ID: 5, Login: "demo-admin", Role: auth.RoleAdmin}, nil
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	token := loginPageToken(t, client, server)

	resp := postForm(t, client, server.URL+"/login", url.Values{
		"login":              {"  Demo-Admin "},
		"password":           {"secret"},
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"))
	assert.Equal(t, "  Demo-Admin ", capturedLogin, "the handler passes the submitted login through; the service normalizes it")
	assert.Equal(t, "secret", capturedPassword)
	assert.Equal(t, 24*time.Hour, capturedTTL, "the session TTL comes from configuration")

	cookie := sessionCookie(resp)
	require.NotNil(t, cookie, "login must set the session cookie")
	assert.Equal(t, strings.Repeat("a", 64), cookie.Value, "the cookie carries the fresh raw token")
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, 24*3600, cookie.MaxAge)
	assert.False(t, cookie.Secure, "COOKIE_SECURE=false must omit the Secure attribute for local HTTP")
}

func TestLoginSecureModeSetsSecureAttribute(t *testing.T) {
	config := testConfig()
	config.CookieSecure = true
	authenticator := &fakeAuthenticator{
		loginFunc: func(context.Context, string, string, time.Duration) (string, auth.User, error) {
			return strings.Repeat("a", 64), auth.User{ID: 5, Login: "demo-admin", Role: auth.RoleAdmin}, nil
		},
	}
	server, client := newTestServer(t, authenticator, config)
	token := loginPageToken(t, client, server)

	// In HTTPS mode the CSRF middleware keeps strict origin checking; a real
	// browser sends the Origin header on form submissions.
	resp := postFormWithHeaders(t, client, server.URL+"/login", url.Values{
		"login":              {"demo-admin"},
		"password":           {"secret"},
		"gorilla.csrf.Token": {token},
	}, map[string]string{"Origin": strings.Replace(server.URL, "http://", "https://", 1)})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	cookie := sessionCookie(resp)
	require.NotNil(t, cookie)
	assert.True(t, cookie.Secure, "COOKIE_SECURE=true must add the Secure attribute")
}

func TestLoginUniformMessageForUnknownLoginAndPassword(t *testing.T) {
	authenticator := &fakeAuthenticator{
		loginFunc: func(context.Context, string, string, time.Duration) (string, auth.User, error) {
			return "", auth.User{}, auth.ErrInvalidCredentials
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	token := loginPageToken(t, client, server)

	for _, login := range []string{"nobody", "demo-admin"} {
		resp := postForm(t, client, server.URL+"/login", url.Values{
			"login":              {login},
			"password":           {"secret"},
			"gorilla.csrf.Token": {token},
		})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "login %q", login)
		assert.Contains(t, resp.Body, "Неверный логин или пароль")
		assert.Nil(t, sessionCookie(resp), "a failed login must not create a session")
		assert.NotContains(t, resp.Body, "secret", "the password must not reappear in the page")
	}
}

func TestLoginDatabaseFailureIsTechnicalError(t *testing.T) {
	authenticator := &fakeAuthenticator{
		loginFunc: func(context.Context, string, string, time.Duration) (string, auth.User, error) {
			return "", auth.User{}, errDatabaseDown
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	token := loginPageToken(t, client, server)

	resp := postForm(t, client, server.URL+"/login", url.Values{
		"login":              {"demo-admin"},
		"password":           {"secret"},
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, resp.Body, "временно недоступен", "a technical failure needs a technical message")
	assert.NotContains(t, resp.Body, "Неверный логин или пароль")
	assert.NotContains(t, resp.Body, "database down", "internal details must not leak")
}

func TestLoginBodyTooLarge(t *testing.T) {
	authenticator := &fakeAuthenticator{
		loginFunc: func(_ context.Context, login, _ string, _ time.Duration) (string, auth.User, error) {
			return strings.Repeat("a", 64), auth.User{ID: 5, Login: login, Role: auth.RoleAdmin}, nil
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	token := loginPageToken(t, client, server)

	resp := postForm(t, client, server.URL+"/login", url.Values{
		"login":              {strings.Repeat("a", 8192)},
		"password":           {"secret"},
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Nil(t, sessionCookie(resp))
}

func TestLoginRequiresValidCSRFToken(t *testing.T) {
	authenticator := &fakeAuthenticator{
		loginFunc: func(context.Context, string, string, time.Duration) (string, auth.User, error) {
			return "", auth.User{}, errLoginReached
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())

	// No token at all.
	resp := postForm(t, client, server.URL+"/login", url.Values{"login": {"demo-admin"}, "password": {"secret"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// A token that does not belong to this browser's cookie.
	resp = postForm(t, client, server.URL+"/login", url.Values{
		"login":              {"demo-admin"},
		"password":           {"secret"},
		"gorilla.csrf.Token": {strings.Repeat("z", 88)},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, resp.Body, "CSRF")
}

func TestAPICSRFFailureIsJSON(t *testing.T) {
	server, client := newTestServer(t, &fakeAuthenticator{}, testConfig())

	resp := postForm(t, client, server.URL+"/api/anything", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &payload))
	assert.Equal(t, "csrf_invalid", payload.Error.Code)
}

func TestLoggedInHomeAndLogout(t *testing.T) {
	sessionToken := strings.Repeat("b", 64)
	authenticator := &fakeAuthenticator{
		userFunc: func(_ context.Context, token string) (auth.User, error) {
			if token == sessionToken {
				return auth.User{ID: 5, Login: "demo-admin", Role: auth.RoleAdmin}, nil
			}

			return auth.User{}, auth.ErrNoSession
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	setSessionCookie(t, client, server.URL, sessionToken)

	resp := getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "demo-admin")
	assert.Contains(t, resp.Body, "администратор", "the navigation shows the role label")
	match := csrfFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the logout form must carry a CSRF field")

	resp = postForm(t, client, server.URL+"/logout", url.Values{"gorilla.csrf.Token": {match[1]}})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
	cookie := sessionCookie(resp)
	require.NotNil(t, cookie, "logout must clear the session cookie")
	assert.LessOrEqual(t, cookie.MaxAge, 0, "the session cookie must be deleted")
	assert.Equal(t, 1, authenticator.logoutCnt, "logout must delete the session row")
}

func TestExpiredSessionBehavesAsGuest(t *testing.T) {
	authenticator := &fakeAuthenticator{
		userFunc: func(context.Context, string) (auth.User, error) {
			return auth.User{}, auth.ErrNoSession
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	setSessionCookie(t, client, server.URL, strings.Repeat("c", 64))

	resp := getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "an expired or substituted session is a guest")
}

func TestSessionCheckDatabaseFailureIs503(t *testing.T) {
	authenticator := &fakeAuthenticator{
		userFunc: func(context.Context, string) (auth.User, error) {
			return auth.User{}, errDatabaseDown
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	setSessionCookie(t, client, server.URL, strings.Repeat("d", 64))

	resp := getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, resp.Body, "временно недоступен")
	assert.NotContains(t, resp.Body, "database down")
}

func TestRequireRoleRejectsInsufficientRole(t *testing.T) {
	handler := &Handler{pages: mustParsePages(t), logger: discardLogger()}
	adminOnly := handler.requireRole(auth.RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	participant := auth.User{ID: 2, Login: "p1", Role: auth.RoleParticipant}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/categories", nil)
	req = req.WithContext(auth.WithUser(req.Context(), participant))
	recorder := httptest.NewRecorder()
	adminOnly.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "Недостаточно прав")

	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/categories", nil)
	req = req.WithContext(auth.WithUser(req.Context(), participant))
	recorder = httptest.NewRecorder()
	adminOnly.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusForbidden, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")

	admin := auth.User{ID: 1, Login: "a1", Role: auth.RoleAdmin}
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/categories", nil)
	req = req.WithContext(auth.WithUser(req.Context(), admin))
	recorder = httptest.NewRecorder()
	adminOnly.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)
}

func TestErrorPageShowsLoggedInNavigation(t *testing.T) {
	sessionToken := strings.Repeat("e", 64)
	authenticator := &fakeAuthenticator{
		userFunc: func(_ context.Context, token string) (auth.User, error) {
			if token == sessionToken {
				return auth.User{ID: 5, Login: "demo-admin", Role: auth.RoleAdmin}, nil
			}

			return auth.User{}, auth.ErrNoSession
		},
	}
	server, client := newTestServer(t, authenticator, testConfig())
	setSessionCookie(t, client, server.URL, sessionToken)

	resp := getRequest(t, client, server.URL+"/missing")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Body, "demo-admin", "the shared layout keeps the role navigation on error pages")
}

func TestLivezSkipsCSRFAndStaysAnonymous(t *testing.T) {
	server, client := newTestServer(t, &fakeAuthenticator{}, testConfig())

	resp := getRequest(t, client, server.URL+"/livez")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok\n", resp.Body)
	assert.Empty(t, resp.Cookies(), "a health check must not receive CSRF cookies")
}
