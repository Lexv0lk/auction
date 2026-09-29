//go:build integration

package httpapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// createFlowBidUser inserts an account directly and returns its ID.
func createFlowBidUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, login, role, plainPassword string) int64 {
	t.Helper()

	hash, err := password.Hash(plainPassword)
	require.NoError(t, err)
	var id int64
	err = pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ($1, $2, $3) RETURNING id",
		login, hash, role).Scan(&id)
	require.NoError(t, err)

	return id
}

// createFlowLot inserts a lot directly in the requested state; finished lots
// get a finished_at to satisfy the result constraint.
func createFlowLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, categoryID int64, status string, startPrice int64, endsAt time.Time) int64 {
	t.Helper()

	finishedAt := "NULL"
	if status == lot.StatusFinished {
		finishedAt = "now()"
	}
	var id int64
	err := pool.QueryRow(ctx,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at, finished_at)"+
			" VALUES ('Лот для ставки', 'Описание лота для ставки', $1, $2, $3, $4, "+finishedAt+") RETURNING id",
		categoryID, startPrice, status, endsAt).Scan(&id)
	require.NoError(t, err)

	return id
}

func bidRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) int64 {
	t.Helper()

	var count int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM bids WHERE lot_id = $1", lotID).Scan(&count))

	return count
}

// pageRequestKey fetches the lot page and extracts the request key the form
// carries, exactly like the no-JavaScript participant does.
func pageRequestKey(t *testing.T, client *http.Client, server *httptest.Server, lotID int64) string {
	t.Helper()

	resp := getRequest(t, client, server.URL+"/lots/"+strconv.FormatInt(lotID, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	match := bidKeyFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the lot page must carry the request key")

	return match[1]
}

func TestBidFlowAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Нумизматика для ставки")
	participantID := createFlowBidUser(t, ctx, pool, "flow-bid-participant", auth.RoleParticipant, "integration-pass")
	createFlowBidUser(t, ctx, pool, "flow-bid-admin", auth.RoleAdmin, "integration-pass")

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), lot.NewService(pool), testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	activeID := createFlowLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))
	dueID := createFlowLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
	draftID := createFlowLot(t, ctx, pool, categoryID, lot.StatusDraft, 100, time.Now().Add(time.Hour))

	participant := loginClient(t, server, "flow-bid-participant", "integration-pass")
	admin := loginClient(t, server, "flow-bid-admin", "integration-pass")
	guest := guestClient(t)
	guestToken := bidAPIToken(t, guest, server.URL)

	// Access control: a guest is redirected, an administrator is refused
	// even with a valid token, a post without CSRF never happens.
	resp := postBidForm(t, guest, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		"100", validBidKey, guestToken)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	adminToken := bidAPIToken(t, admin, server.URL)
	resp = postBidForm(t, admin, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		"100", validBidKey, adminToken)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "an administrator must not bid")

	resp = postFormWithHeaders(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		url.Values{"amount": {"100"}, "request_key": {validBidKey}}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the form post carries no CSRF token")
	assert.Zero(t, bidRowCount(t, ctx, pool, activeID), "refused requests never write bids")

	// The no-JavaScript form flow: the key comes from the rendered page, the
	// first submission is accepted and shown on the redirected page.
	key := pageRequestKey(t, participant, server, activeID)
	resp = postBidForm(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)+"/bids", "100", key, pageCSRFToken(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	placedLocation := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(placedLocation, "/lots/"+strconv.FormatInt(activeID, 10)+"?placed="), placedLocation)

	var storedID, storedUser, storedAmount int64
	var storedKey string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT id, user_id, amount, request_key FROM bids WHERE lot_id = $1", activeID).
		Scan(&storedID, &storedUser, &storedAmount, &storedKey))
	assert.Equal(t, participantID, storedUser, "the bid belongs to the session participant")
	assert.Equal(t, int64(100), storedAmount)
	assert.Equal(t, key, storedKey)

	resp = getRequest(t, participant, server.URL+placedLocation)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "принята")
	assert.Contains(t, resp.Body, ">100<")

	// A resubmission of the same form repeats the same intention: the same
	// record, the explicit replay marker, no second row.
	resp = postBidForm(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)+"/bids", "100", key, pageCSRFToken(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeID, 10)))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "/lots/"+strconv.FormatInt(activeID, 10)+"?replayed="),
		"the repeat is answered as a replay: "+resp.Header.Get("Location"))
	assert.Equal(t, int64(1), bidRowCount(t, ctx, pool, activeID))

	// The API answers the same intention with the replayed marker and the
	// stored bid; the repeat still works through the other route.
	apiToken := bidAPIToken(t, participant, server.URL)
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		bidAPIBody("100", key), map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var replayBody struct {
		Bid struct {
			ID     string `json:"id"`
			Amount string `json:"amount"`
		} `json:"bid"`
		Replayed bool `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &replayBody))
	assert.True(t, replayBody.Replayed)
	assert.Equal(t, strconv.FormatInt(storedID, 10), replayBody.Bid.ID)
	assert.Equal(t, int64(1), bidRowCount(t, ctx, pool, activeID))

	// A new key with a higher amount is a new bid; a guest API call stays a
	// JSON 401 even with a token.
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		bidAPIBody("101", validBidKey), map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, int64(2), bidRowCount(t, ctx, pool, activeID))

	resp = postJSON(t, guest, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		bidAPIBody("150", validBidKey), map[string]string{"X-CSRF-Token": guestToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Confirmed refusals: an equal amount is too low, the stored key with
	// another amount is a conflict, and neither writes a row.
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		bidAPIBody("101", "b1000000-0000-4000-8000-000000000001"),
		map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "bid_too_low")

	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		bidAPIBody("999", validBidKey), map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "request_key_conflict")
	assert.Equal(t, int64(2), bidRowCount(t, ctx, pool, activeID))

	// Malformed bodies and identity sabotage are refused before any write.
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		`{"amount":"101","request_key":"`+validBidKey+`","user_id":"`+strconv.FormatInt(createFlowSaboteurID(t, ctx, pool), 10)+`"}`,
		map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, resp.Body, "invalid_body")
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(activeID, 10)+"/bids",
		`{"amount":101,"request_key":"`+validBidKey+`"}`, map[string]string{"X-CSRF-Token": apiToken})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, int64(2), bidRowCount(t, ctx, pool, activeID))

	// The deadline decides, not the open form: a lot past its deadline
	// refuses both shapes of submission with the closed outcome.
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(dueID, 10)+"/bids",
		bidAPIBody("100", validBidKey), map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "auction_closed")

	resp = postBidForm(t, participant, server.URL+"/lots/"+strconv.FormatInt(dueID, 10)+"/bids", "100", validBidKey, pageCSRFToken(t, participant, server.URL+"/lots/"+strconv.FormatInt(dueID, 10)))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "торги закрыты")
	assert.Zero(t, bidRowCount(t, ctx, pool, dueID))

	// A draft is invisible for the participant: both routes answer 404 like
	// a missing lot and write nothing.
	resp = postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(draftID, 10)+"/bids",
		bidAPIBody("100", validBidKey), map[string]string{"X-CSRF-Token": apiToken})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = postBidForm(t, participant, server.URL+"/lots/"+strconv.FormatInt(draftID, 10)+"/bids", "100", validBidKey, apiToken)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Zero(t, bidRowCount(t, ctx, pool, draftID))
}

func createCategoryRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ($1) RETURNING id", name).Scan(&id)
	require.NoError(t, err)

	return id
}

func createFlowSaboteurID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ('flow-bid-saboteur', 'integration-hash', 'participant') RETURNING id",
	).Scan(&id)
	require.NoError(t, err)

	return id
}

// guestClient returns a browser-like client without a session: the cookie
// jar still accumulates the CSRF cookie, exactly like a real guest.
func guestClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           jar,
	}
}
