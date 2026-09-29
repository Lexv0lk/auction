//go:build integration

package httpapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// TestLoginFlowAgainstPostgreSQL walks the full browser flow through the real
// session service: CSRF-protected login, role navigation, hashed storage and
// logout. Substituted cookies must close the access.
func TestLoginFlowAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	hash, err := password.Hash("integration-pass")
	require.NoError(t, err)
	var userID int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ('flow-admin', $1, 'admin') RETURNING id",
		hash).Scan(&userID))

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), lot.NewService(pool), testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           jar,
	}

	// Wrong password and unknown login never create a session.
	token := loginPageToken(t, client, server)
	resp := postForm(t, client, server.URL+"/login", url.Values{
		"login": {"flow-admin"}, "password": {"wrong"}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Body, "Неверный логин или пароль")
	resp = postForm(t, client, server.URL+"/login", url.Values{
		"login": {"ghost"}, "password": {"integration-pass"}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "Неверный логин или пароль", loginErrorText(t, resp), "both failures share one message")

	var sessions int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id = $1", userID).Scan(&sessions))
	assert.Zero(t, sessions, "failed logins must not create sessions")

	// A successful login issues a fresh cookie and stores only the hash.
	token = loginPageToken(t, client, server)
	resp = postForm(t, client, server.URL+"/login", url.Values{
		"login": {" flow-admin "}, "password": {"integration-pass"}, "gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	session := sessionCookie(resp)
	require.NotNil(t, session)
	require.Len(t, session.Value, 64)

	sum := sha256.Sum256([]byte(session.Value))
	var storedHash string
	require.NoError(t, pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE user_id = $1", userID).Scan(&storedHash))
	assert.Equal(t, hex.EncodeToString(sum[:]), storedHash)
	assert.NotEqual(t, session.Value, storedHash, "no raw cookie token in the database")

	// The session works: the home page greets the user by role.
	resp = getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "flow-admin")

	// A substituted cookie of a well-formed but unknown token is a guest.
	anonymous, err := cookiejar.New(nil)
	require.NoError(t, err)
	anonymous.SetCookies(mustParseURL(t, server.URL), []*http.Cookie{
		{Name: "auction_session", Value: strings.Repeat("9", 64)},
	})
	stranger := &http.Client{
		CheckRedirect: client.CheckRedirect,
		Jar:           anonymous,
	}
	resp = getRequest(t, stranger, server.URL+"/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "substituted cookies must not authenticate")

	// Logout deletes the row and closes the access.
	body := getRequest(t, client, server.URL+"/")
	tokenFromHome := csrfFieldPattern.FindStringSubmatch(body.Body)
	require.NotNil(t, tokenFromHome)
	resp = postForm(t, client, server.URL+"/logout", url.Values{"gorilla.csrf.Token": {tokenFromHome[1]}})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id = $1", userID).Scan(&sessions))
	assert.Zero(t, sessions, "logout must delete the session row")

	resp = getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "after logout the user is a guest again")
}

func loginErrorText(t *testing.T, resp *httpResponse) string {
	t.Helper()

	for _, line := range strings.Split(resp.Body, "\n") {
		if strings.Contains(line, "class=\"error\"") {
			return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "<p class=\"error\">"), "</p>"))
		}
	}

	return ""
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	require.NoError(t, err)

	return parsed
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}
