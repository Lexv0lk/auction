package httpapp

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
)

// fakeAuthenticator records how the HTTP layer drives the session service.
type fakeAuthenticator struct {
	loginFunc func(ctx context.Context, login, password string, ttl time.Duration) (string, auth.User, error)
	userFunc  func(ctx context.Context, token string) (auth.User, error)
	logoutCnt int
	logoutErr error
}

func (f *fakeAuthenticator) Login(ctx context.Context, login, password string, ttl time.Duration) (string, auth.User, error) {
	return f.loginFunc(ctx, login, password, ttl)
}

func (f *fakeAuthenticator) User(ctx context.Context, token string) (auth.User, error) {
	return f.userFunc(ctx, token)
}

func (f *fakeAuthenticator) Logout(_ context.Context, _ string) error {
	f.logoutCnt++

	return f.logoutErr
}

// httpResponse is a fully read HTTP response: the body is consumed and the
// connection released inside the helpers.
type httpResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
	cookies    []*http.Cookie
}

// Cookies returns the cookies of the Set-Cookie headers.
func (r *httpResponse) Cookies() []*http.Cookie {
	return r.cookies
}

var testCSRFKey = []byte("unit-test-csrf-key-unit-test-csrf-")

func testConfig() Config {
	return Config{SessionTTL: 24 * time.Hour, CookieSecure: false, CSRFKey: testCSRFKey}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func mustParsePages(t *testing.T) map[string]*template.Template {
	t.Helper()

	pages, err := parsePageTemplates()
	require.NoError(t, err)

	return pages
}

func newTestHandler(t *testing.T, authenticator Authenticator, config Config) http.Handler {
	t.Helper()

	handler, err := NewHandler(discardLogger(), authenticator, config)
	require.NoError(t, err)

	return handler
}

// newTestServer runs the full handler stack behind a real HTTP server with a
// cookie jar, mirroring how a browser accumulates CSRF and session cookies.
func newTestServer(t *testing.T, authenticator Authenticator, config Config) (*httptest.Server, *http.Client) {
	t.Helper()

	server := httptest.NewServer(newTestHandler(t, authenticator, config))
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           jar,
	}

	return server, client
}

func doRequest(t *testing.T, client *http.Client, req *http.Request) *httpResponse {
	t.Helper()

	resp, err := client.Do(req) //nolint:bodyclose // the helper reads and closes the body itself
	require.NoError(t, err)
	cookies := resp.Cookies()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return &httpResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: string(body), cookies: cookies}
}

func getRequest(t *testing.T, client *http.Client, target string) *httpResponse {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	require.NoError(t, err)

	return doRequest(t, client, req)
}

func postFormWithHeaders(t *testing.T, client *http.Client, target string, values url.Values, headers map[string]string) *httpResponse {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	return doRequest(t, client, req)
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *httpResponse {
	t.Helper()

	return postFormWithHeaders(t, client, target, values, nil)
}

var csrfFieldPattern = regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`)

// loginPageToken fetches the login page like a browser and returns the CSRF
// token the rendered form carries.
func loginPageToken(t *testing.T, client *http.Client, server *httptest.Server) string {
	t.Helper()

	resp := getRequest(t, client, server.URL+"/login")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	match := csrfFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "login page must carry a CSRF field")

	return match[1]
}
