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
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
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

// fakeCategories records how the HTTP layer drives the category service.
type fakeCategories struct {
	listFunc   func(ctx context.Context) ([]category.Category, error)
	createFunc func(ctx context.Context, name string) (category.Category, error)
	getFunc    func(ctx context.Context, id int64) (category.Category, error)
	renameFunc func(ctx context.Context, id int64, name string) (category.Category, error)
	deleteFunc func(ctx context.Context, id int64) error

	listCalls   int
	createCalls int
	renameCalls int
	deleteCalls int
}

func (f *fakeCategories) List(ctx context.Context) ([]category.Category, error) {
	f.listCalls++
	if f.listFunc == nil {
		return nil, nil
	}

	return f.listFunc(ctx)
}

func (f *fakeCategories) Create(ctx context.Context, name string) (category.Category, error) {
	f.createCalls++
	if f.createFunc == nil {
		return category.Category{}, nil
	}

	return f.createFunc(ctx, name)
}

func (f *fakeCategories) Get(ctx context.Context, id int64) (category.Category, error) {
	if f.getFunc == nil {
		return category.Category{}, nil
	}

	return f.getFunc(ctx, id)
}

func (f *fakeCategories) Rename(ctx context.Context, id int64, name string) (category.Category, error) {
	f.renameCalls++
	if f.renameFunc == nil {
		return category.Category{}, nil
	}

	return f.renameFunc(ctx, id, name)
}

func (f *fakeCategories) Delete(ctx context.Context, id int64) error {
	f.deleteCalls++
	if f.deleteFunc == nil {
		return nil
	}

	return f.deleteFunc(ctx, id)
}

// fakeLots records how the HTTP layer drives the lot service.
type fakeLots struct {
	listFunc    func(ctx context.Context) ([]lot.Lot, error)
	getFunc     func(ctx context.Context, id int64) (lot.Lot, error)
	createFunc  func(ctx context.Context, input lot.Input) (lot.Lot, error)
	updateFunc  func(ctx context.Context, id int64, input lot.Input) (lot.Lot, error)
	deleteFunc  func(ctx context.Context, id int64) error
	publishFunc func(ctx context.Context, id int64) (lot.Lot, error)
	catalogFunc func(ctx context.Context, filter lot.CatalogFilter) ([]lot.CatalogItem, bool, error)
	publicFunc  func(ctx context.Context, id int64) (lot.PublicLot, error)
	bidsFunc    func(ctx context.Context, lotID int64, page int) ([]lot.Bid, bool, error)
	placeFunc   func(ctx context.Context, participantID, lotID int64, amount int64, requestKey string) (lot.PlacedBid, error)

	listCalls    int
	createCalls  int
	updateCalls  int
	deleteCalls  int
	publishCalls int
	catalogCalls int
	publicCalls  int
	bidsCalls    int
	placeCalls   int

	lastFilter lot.CatalogFilter
	lastInput  lot.Input
	lastID     int64
	lastPage   int
	lastPlace  placeCall
}

// placeCall records one PlaceBid invocation: the participant always comes
// from the session, so a test can prove the body never overrides it.
type placeCall struct {
	ParticipantID int64
	LotID         int64
	Amount        int64
	RequestKey    string
}

func (f *fakeLots) List(ctx context.Context) ([]lot.Lot, error) {
	f.listCalls++
	if f.listFunc == nil {
		return nil, nil
	}

	return f.listFunc(ctx)
}

func (f *fakeLots) Get(ctx context.Context, id int64) (lot.Lot, error) {
	f.lastID = id
	if f.getFunc == nil {
		return lot.Lot{}, nil
	}

	return f.getFunc(ctx, id)
}

func (f *fakeLots) Create(ctx context.Context, input lot.Input) (lot.Lot, error) {
	f.createCalls++
	f.lastInput = input
	if f.createFunc == nil {
		return lot.Lot{}, nil
	}

	return f.createFunc(ctx, input)
}

func (f *fakeLots) Update(ctx context.Context, id int64, input lot.Input) (lot.Lot, error) {
	f.updateCalls++
	f.lastID = id
	f.lastInput = input
	if f.updateFunc == nil {
		return lot.Lot{}, nil
	}

	return f.updateFunc(ctx, id, input)
}

func (f *fakeLots) Delete(ctx context.Context, id int64) error {
	f.deleteCalls++
	f.lastID = id
	if f.deleteFunc == nil {
		return nil
	}

	return f.deleteFunc(ctx, id)
}

func (f *fakeLots) Publish(ctx context.Context, id int64) (lot.Lot, error) {
	f.publishCalls++
	f.lastID = id
	if f.publishFunc == nil {
		return lot.Lot{}, nil
	}

	return f.publishFunc(ctx, id)
}

func (f *fakeLots) Catalog(ctx context.Context, filter lot.CatalogFilter) ([]lot.CatalogItem, bool, error) {
	f.catalogCalls++
	f.lastFilter = filter
	if f.catalogFunc == nil {
		return nil, false, nil
	}

	return f.catalogFunc(ctx, filter)
}

func (f *fakeLots) GetPublicLot(ctx context.Context, id int64) (lot.PublicLot, error) {
	f.publicCalls++
	f.lastID = id
	if f.publicFunc == nil {
		return lot.PublicLot{}, nil
	}

	return f.publicFunc(ctx, id)
}

func (f *fakeLots) ListBids(ctx context.Context, lotID int64, page int) ([]lot.Bid, bool, error) {
	f.bidsCalls++
	f.lastID = lotID
	f.lastPage = page
	if f.bidsFunc == nil {
		return nil, false, nil
	}

	return f.bidsFunc(ctx, lotID, page)
}

func (f *fakeLots) PlaceBid(ctx context.Context, participantID, lotID int64, amount int64, requestKey string) (lot.PlacedBid, error) {
	f.placeCalls++
	f.lastPlace = placeCall{ParticipantID: participantID, LotID: lotID, Amount: amount, RequestKey: requestKey}
	if f.placeFunc == nil {
		return lot.PlacedBid{}, nil
	}

	return f.placeFunc(ctx, participantID, lotID, amount, requestKey)
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

// newTestHandler builds the full handler stack with fake services; a test
// substitutes its own dependency by passing it as a dependency argument
// (anything unrecognized is ignored).
func newTestHandler(t *testing.T, authenticator Authenticator, config Config, deps ...any) http.Handler {
	t.Helper()

	var categories Categories = &fakeCategories{}
	var lots Lots = &fakeLots{}
	var metrics Metrics
	for _, dep := range deps {
		switch dep := dep.(type) {
		case Categories:
			categories = dep
		case Lots:
			lots = dep
		case Metrics:
			metrics = dep
		}
	}
	handler, err := NewHandler(discardLogger(), authenticator, categories, lots, metrics, config)
	require.NoError(t, err)

	return handler
}

// newTestServer runs the full handler stack behind a real HTTP server with a
// cookie jar, mirroring how a browser accumulates CSRF and session cookies.
// The services default to stubs; tests pass their own fakes as dependencies.
func newTestServer(t *testing.T, authenticator Authenticator, config Config, deps ...any) (*httptest.Server, *http.Client) {
	t.Helper()

	server := httptest.NewServer(newTestHandler(t, authenticator, config, deps...))
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
