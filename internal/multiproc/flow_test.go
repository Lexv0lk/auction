//go:build integration

package multiproc

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/password"
)

// page is a fully read HTTP response.
type page struct {
	Status int
	Header http.Header
	Body   string
}

// newClient returns a browser-like client: a cookie jar (session + CSRF
// cookies) and no redirect following, so every 303 stays visible.
func newClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           jar,
	}
}

var csrfFieldPattern = regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`)

// bidKeyFieldPattern extracts the request key of the rendered bid form.
var bidKeyFieldPattern = regexp.MustCompile(`name="request_key"[^>]*value="([0-9a-f-]{36})"`)

// pageToken fetches a page and returns the CSRF token of its form. A token
// taken from /login covers every post (the cookie there has the default path
// "/"), tokens from inner pages cover their section only.
func pageToken(t *testing.T, client *http.Client, target string) string {
	t.Helper()

	resp := get(t, client, target)
	require.Equal(t, http.StatusOK, resp.Status, "GET %s must render the form", target)
	match := csrfFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the page %s must carry a CSRF field", target)

	return match[1]
}

// login walks the real login flow: the CSRF token of the login page, the
// form post, the redirect to the home page.
func login(t *testing.T, client *http.Client, base, name, secret string) {
	t.Helper()

	token := pageToken(t, client, base+"/login")
	resp := postForm(t, client, base+"/login", url.Values{
		"login":              {name},
		"password":           {secret},
		"gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.Status, "the login of %s must succeed", name)
}

func get(t *testing.T, client *http.Client, target string) page {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	require.NoError(t, err)

	return do(t, client, req)
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) page {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return do(t, client, req)
}

func postJSON(t *testing.T, client *http.Client, target, payload string, headers map[string]string) page {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	return do(t, client, req)
}

func do(t *testing.T, client *http.Client, req *http.Request) page {
	t.Helper()

	resp, err := client.Do(req) //nolint:bodyclose // the helper reads and closes the body itself
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	return page{Status: resp.StatusCode, Header: resp.Header, Body: string(body)}
}

// pageBidKey fetches the lot page and returns the request key of its form,
// exactly like the no-JavaScript participant does.
func pageBidKey(t *testing.T, client *http.Client, target string) string {
	t.Helper()

	resp := get(t, client, target)
	require.Equal(t, http.StatusOK, resp.Status, "GET %s must render the lot page", target)
	match := bidKeyFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the lot page %s must carry the request key", target)

	return match[1]
}

// bidAPIBody builds the JSON body of the bid API: money and keys are strings.
func bidAPIBody(amount, key string) string {
	return `{"amount":"` + amount + `","request_key":"` + key + `"}`
}

// --- database fixtures and checks -------------------------------------------

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return ctx
}

// createUser inserts an account that can log in through HTTP.
func createUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, role, secret string) int64 {
	t.Helper()

	hash, err := password.Hash(secret)
	require.NoError(t, err)
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ($1, $2, $3) RETURNING id",
		name, hash, role).Scan(&id))

	return id
}

func createCategoryRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO categories (name) VALUES ($1) RETURNING id", name).Scan(&id))

	return id
}

// createLotRow inserts a lot in the requested state; finished lots get a
// finished_at to satisfy the result constraint.
func createLotRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, categoryID int64, status string, startPrice int64, endsAt time.Time) int64 {
	t.Helper()

	finishedAt := "NULL"
	if status == lot.StatusFinished {
		finishedAt = "now()"
	}
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at, finished_at)"+
			" VALUES ('Многопроцессный лот', 'Описание многопроцессного лота', $1, $2, $3, $4, "+finishedAt+") RETURNING id",
		categoryID, startPrice, status, endsAt).Scan(&id))

	return id
}

// setLotDeadline moves the deadline of a published lot: the scenarios run the
// competition on a live HTTP surface and then hand the lot to the workers.
func setLotDeadline(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64, endsAt time.Time) {
	t.Helper()

	tag, err := pool.Exec(ctx, "UPDATE lots SET ends_at = $1 WHERE id = $2", endsAt, lotID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "the lot must exist")
}

// insertBidRow writes an accepted bid directly, imitating a bid committed
// before the deadline.
func insertBidRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID, userID, amount int64, acceptedAt time.Time, key string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		lotID, userID, amount, acceptedAt, key).Scan(&id))

	return id
}

func countBids(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) int64 {
	t.Helper()

	var count int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM bids WHERE lot_id = $1", lotID).Scan(&count))

	return count
}

func maxBidAmount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) int64 {
	t.Helper()

	var amount int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(amount), 0) FROM bids WHERE lot_id = $1", lotID).Scan(&amount))

	return amount
}

// lotRowState is the stored result of a lot as the scenarios read it back.
type lotRowState struct {
	status       string
	finishedAt   *time.Time
	winningBidID *int64
}

func readLotState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) lotRowState {
	t.Helper()

	var state lotRowState
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status, finished_at, winning_bid_id FROM lots WHERE id = $1", lotID).
		Scan(&state.status, &state.finishedAt, &state.winningBidID))

	return state
}

// waitLotFinished polls the stored state until the background loop has
// committed the result.
func waitLotFinished(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) lotRowState {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		state := readLotState(t, ctx, pool, lotID)
		if state.status == lot.StatusFinished {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("lot %d was never finished (stayed %s)", lotID, state.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitFor polls a condition until it holds; the failure names what never
// happened.
func waitFor(t *testing.T, timeout time.Duration, what string, probe func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if probe() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// formatLotDeadline renders a deadline for the admin lot form.
func formatLotDeadline(at time.Time) string {
	return at.Format("2006-01-02T15:04")
}

// parseInt64 parses a decimal identifier out of a redirect target.
func parseInt64(t *testing.T, value string) int64 {
	t.Helper()

	id, err := strconv.ParseInt(value, 10, 64)
	require.NoError(t, err)

	return id
}
