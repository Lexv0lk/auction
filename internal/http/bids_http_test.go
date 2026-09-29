package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/lot"
)

const validBidKey = "6f1c0e64-1f2d-4c1a-9e5e-8d2b3c4a5f6b"

var errCommitLost = errors.New("bid commit outcome unknown")

func commitLostError() error {
	return fmt.Errorf("commit bid transaction: %w: %w", lot.ErrCommitOutcomeUnknown, errCommitLost)
}

func activeBidLot() lot.PublicLot {
	return lot.PublicLot{
		ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
		StartPrice: 100, Status: lot.StatusActive, State: lot.DisplayStateActive,
		CurrentPrice: 100, MaxBid: 0,
		EndsAt: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
	}
}

func placedBid(repeated bool) lot.PlacedBid {
	return lot.PlacedBid{
		ID: 42, LotID: 7, ParticipantID: 2, Amount: 101,
		AcceptedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		RequestKey: validBidKey,
		Repeated:   repeated,
	}
}

func postJSON(t *testing.T, client *http.Client, target, payload string, headers map[string]string) *httpResponse {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	return doRequest(t, client, req)
}

// bidAPIToken takes the CSRF token from /login: the cookie issued there has
// the default path "/", so it covers both /lots/* and /api/lots/* posts.
func bidAPIToken(t *testing.T, client *http.Client, serverURL string) string {
	t.Helper()

	return pageCSRFToken(t, client, serverURL+"/login")
}

func bidAPIBody(amount, key string) string {
	return `{"amount":"` + amount + `","request_key":"` + key + `"}`
}

func TestBidAPIAccessControl(t *testing.T) {
	lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return placedBid(false), nil
	}}
	server, guest := newTestServer(t, &fakeAuthenticator{}, testConfig(), lots)

	token := bidAPIToken(t, guest, server.URL)
	resp := postJSON(t, guest, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey),
		map[string]string{"X-CSRF-Token": token})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a guest is answered with JSON 401")
	assert.Contains(t, resp.Body, `"unauthenticated"`)
	assert.Zero(t, lots.placeCalls)

	server, admin := adminClient(t, lots)
	token = bidAPIToken(t, admin, server.URL)
	resp = postJSON(t, admin, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey),
		map[string]string{"X-CSRF-Token": token})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "an administrator must not bid")
	assert.Contains(t, resp.Body, `"forbidden"`)
	assert.Zero(t, lots.placeCalls)

	server, participant := participantClient(t, lots)
	resp = postJSON(t, participant, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey), nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a bid without the CSRF token is rejected before any work")
	assert.Contains(t, resp.Body, `"csrf_invalid"`)
	assert.Zero(t, lots.placeCalls)
}

func TestBidAPICreatesAndReplays(t *testing.T) {
	lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return placedBid(false), nil
	}}
	server, participant := participantClient(t, lots)
	token := bidAPIToken(t, participant, server.URL)

	resp := postJSON(t, participant, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	assert.Equal(t, placeCall{ParticipantID: 2, LotID: 7, Amount: 101, RequestKey: validBidKey}, lots.lastPlace,
		"the participant comes from the session, the amount and the key from the body")

	var created struct {
		Bid struct {
			ID         string `json:"id"`
			LotID      string `json:"lot_id"`
			Amount     string `json:"amount"`
			AcceptedAt string `json:"accepted_at"`
		} `json:"bid"`
		Replayed bool `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &created))
	assert.Equal(t, "42", created.Bid.ID, "identifiers and money are decimal strings")
	assert.Equal(t, "7", created.Bid.LotID)
	assert.Equal(t, "101", created.Bid.Amount)
	assert.Equal(t, "2026-09-29T12:00:00Z", created.Bid.AcceptedAt)
	assert.False(t, created.Replayed)
	assert.NotContains(t, resp.Body, "request_key", "the request key never leaves the database")

	lots.placeFunc = func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return placedBid(true), nil
	}
	resp = postJSON(t, participant, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusOK, resp.StatusCode, "a replayed bid is answered with 200")
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &created))
	assert.Equal(t, "42", created.Bid.ID, "the replay returns the stored bid, the same ID")
	assert.True(t, created.Replayed)
}

type bidAPIErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestBidAPIMapsServiceOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{"too low", lot.ErrBidTooLow, http.StatusConflict, "bid_too_low"},
		{"closed", lot.ErrBiddingClosed, http.StatusConflict, "auction_closed"},
		{"key conflict", lot.ErrRequestKeyConflict, http.StatusConflict, "request_key_conflict"},
		{"missing lot", lot.ErrNotFound, http.StatusNotFound, "lot_not_found"},
		{"bad amount", lot.ErrBidAmountInvalid, http.StatusUnprocessableEntity, "invalid_bid"},
		{"bad key", lot.ErrRequestKeyInvalid, http.StatusUnprocessableEntity, "invalid_request_key"},
		{"no participant", lot.ErrParticipantRequired, http.StatusForbidden, "forbidden"},
		{"participant gone", lot.ErrParticipantMissing, http.StatusForbidden, "forbidden"},
		{"commit unknown", commitLostError(), http.StatusServiceUnavailable, "commit_outcome_unknown"},
		{"database down", errLotsDown, http.StatusServiceUnavailable, "service_unavailable"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
				return lot.PlacedBid{}, testCase.serviceErr
			}}
			server, participant := participantClient(t, lots)
			token := bidAPIToken(t, participant, server.URL)

			resp := postJSON(t, participant, server.URL+"/api/lots/7/bids", bidAPIBody("101", validBidKey),
				map[string]string{"X-CSRF-Token": token})
			require.Equal(t, testCase.wantStatus, resp.StatusCode)

			var body bidAPIErrorBody
			require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
			assert.Equal(t, testCase.wantCode, body.Error.Code)
			assert.NotEmpty(t, body.Error.Message, "every refusal explains itself to a human")
			assert.NotContains(t, resp.Body, "fake database down", "technical details stay in the log")
			assert.NotContains(t, resp.Body, "SELECT")
		})
	}
}

func TestBidAPIValidatesBody(t *testing.T) {
	cases := []struct {
		name           string
		payload        string
		wantStatus     int
		wantCode       string
		wantPlaceCalls int
	}{
		{"empty object", `{}`, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"empty body", ``, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"malformed json", `{"amount":`, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"number amount", `{"amount":101,"request_key":"` + validBidKey + `"}`, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"missing key", `{"amount":"101"}`, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"unknown field", `{"amount":"101","request_key":"` + validBidKey + `","user_id":"9"}`, http.StatusUnprocessableEntity, "invalid_body", 0},
		{"fraction", bidAPIBody("10.5", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		{"letters", bidAPIBody("abc", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		{"negative", bidAPIBody("-101", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		{"zero", bidAPIBody("0", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		{"plus sign", bidAPIBody("+101", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		{"overflow", bidAPIBody("9223372036854775808", validBidKey), http.StatusUnprocessableEntity, "invalid_bid", 0},
		// The key is validated by the service itself: the single source of
		// truth for the UUID contract stays in the lot package.
		{"bad key", bidAPIBody("101", "not-a-uuid"), http.StatusUnprocessableEntity, "invalid_request_key", 1},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
				return lot.PlacedBid{}, lot.ErrRequestKeyInvalid
			}}
			server, participant := participantClient(t, lots)
			token := bidAPIToken(t, participant, server.URL)

			resp := postJSON(t, participant, server.URL+"/api/lots/7/bids", testCase.payload,
				map[string]string{"X-CSRF-Token": token})
			require.Equal(t, testCase.wantStatus, resp.StatusCode)

			var body bidAPIErrorBody
			require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
			assert.Equal(t, testCase.wantCode, body.Error.Code)
			assert.Equal(t, testCase.wantPlaceCalls, lots.placeCalls, "a rejected body reaches the service only through the key check")
		})
	}
}

func TestBidAPILimitsBody(t *testing.T) {
	lots := &fakeLots{}
	server, participant := participantClient(t, lots)
	token := bidAPIToken(t, participant, server.URL)

	huge := `{"amount":"101","request_key":"` + validBidKey + `","pad":"` + strings.Repeat("x", 8192) + `"}`
	resp := postJSON(t, participant, server.URL+"/api/lots/7/bids", huge,
		map[string]string{"X-CSRF-Token": token})
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Contains(t, resp.Body, "request_too_large")
	assert.Zero(t, lots.placeCalls)
}

func TestBidAPIMalformedRouteID(t *testing.T) {
	server, participant := participantClient(t)
	token := bidAPIToken(t, participant, server.URL)

	resp := postJSON(t, participant, server.URL+"/api/lots/bad/bids", bidAPIBody("101", validBidKey),
		map[string]string{"X-CSRF-Token": token})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

var bidKeyFieldPattern = regexp.MustCompile(`name="request_key"[^>]*value="([0-9a-f-]{36})"`)

// bidFormSetup renders the lot page like a browser and returns it together
// with the fresh request key and the CSRF token the page carries.
func bidFormSetup(t *testing.T, lots *fakeLots) (*httptest.Server, *http.Client, string, string) {
	t.Helper()

	server, participant := participantClient(t, lots)
	resp := getRequest(t, participant, server.URL+"/lots/7")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	keyMatch := bidKeyFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, keyMatch, "the lot page must carry the request key of a fresh bid intention")
	tokenMatch := csrfFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, tokenMatch, "the lot page must carry the CSRF field")

	return server, participant, keyMatch[1], tokenMatch[1]
}

// postBidForm submits the bid form the way the rendered page would.
func postBidForm(t *testing.T, client *http.Client, target, amount, key, token string) *httpResponse {
	t.Helper()

	return postFormWithHeaders(t, client, target,
		url.Values{"amount": {amount}, "request_key": {key}, "gorilla.csrf.Token": {token}}, nil)
}

func TestBidFormFlow(t *testing.T) {
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) { return activeBidLot(), nil },
		bidsFunc: func(context.Context, int64, int) ([]lot.Bid, bool, error) {
			return []lot.Bid{}, false, nil
		},
		placeFunc: func(_ context.Context, participantID, lotID, amount int64, _ string) (lot.PlacedBid, error) {
			require.Equal(t, int64(2), participantID, "the form bids as the session participant")
			require.Equal(t, int64(7), lotID)
			require.Equal(t, int64(101), amount)

			return placedBid(false), nil
		},
	}
	server, participant, key, token := bidFormSetup(t, lots)

	resp := postBidForm(t, participant, server.URL+"/lots/7/bids", "101", key, token)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/lots/7?placed=42", resp.Header.Get("Location"), "success redirects to the lot page")

	// The page shows the confirmed outcome from the query marker.
	resp = getRequest(t, participant, server.URL+"/lots/7?placed=42")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "принята")
	assert.NotContains(t, resp.Body, "Ошибка", "a success marker is not an error")

	// A no-JavaScript resubmission of the same form repeats the same
	// intention: one record, the stored bid, an explicit replay marker.
	lots.placeFunc = func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return placedBid(true), nil
	}
	resp = postBidForm(t, participant, server.URL+"/lots/7/bids", "101", key, token)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/lots/7?replayed=42", resp.Header.Get("Location"))

	resp = getRequest(t, participant, server.URL+"/lots/7?replayed=42")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "повтор")

	// A garbage marker is ignored, not echoed.
	resp = getRequest(t, participant, server.URL+"/lots/7?placed=<script>")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, resp.Body, "<script>")
}

func TestBidFormPreservesInputOnFailure(t *testing.T) {
	cases := []struct {
		name        string
		amount      string
		key         string
		placeErr    error
		wantStatus  int
		wantMessage string
	}{
		{
			name: "too low", amount: "101", key: validBidKey, placeErr: lot.ErrBidTooLow,
			wantStatus: http.StatusConflict, wantMessage: "ниже минимальной",
		},
		{
			name: "closed", amount: "101", key: validBidKey, placeErr: lot.ErrBiddingClosed,
			wantStatus: http.StatusConflict, wantMessage: "торги закрыты",
		},
		{
			name: "key conflict", amount: "101", key: validBidKey, placeErr: lot.ErrRequestKeyConflict,
			wantStatus: http.StatusConflict, wantMessage: "другой суммой",
		},
		{
			name: "invalid amount", amount: "abc", key: validBidKey, placeErr: nil,
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "целым положительным числом",
		},
		{
			name: "zero amount", amount: "0", key: validBidKey, placeErr: nil,
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "целым положительным числом",
		},
		{
			name: "invalid key", amount: "101", key: "not-a-uuid", placeErr: lot.ErrRequestKeyInvalid,
			wantStatus: http.StatusUnprocessableEntity, wantMessage: "ключ запроса",
		},
		{
			name: "unknown outcome", amount: "101", key: validBidKey, placeErr: commitLostError(),
			wantStatus: http.StatusServiceUnavailable, wantMessage: "повторите отправку той же суммы",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lots := &fakeLots{
				publicFunc: func(context.Context, int64) (lot.PublicLot, error) { return activeBidLot(), nil },
				bidsFunc: func(context.Context, int64, int) ([]lot.Bid, bool, error) {
					return []lot.Bid{}, false, nil
				},
				placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
					return lot.PlacedBid{}, testCase.placeErr
				},
			}
			server, participant, _, token := bidFormSetup(t, lots)

			resp := postBidForm(t, participant, server.URL+"/lots/7/bids", testCase.amount, testCase.key, token)
			require.Equal(t, testCase.wantStatus, resp.StatusCode)
			assert.Contains(t, resp.Body, testCase.wantMessage)
			assert.Contains(t, resp.Body, `value="`+testCase.amount+`"`, "the entered amount comes back")
			if errors.Is(testCase.placeErr, lot.ErrRequestKeyInvalid) {
				assert.NotContains(t, resp.Body, `value="`+testCase.key+`"`, "an unusable key is replaced")
			} else {
				assert.Contains(t, resp.Body, `value="`+testCase.key+`"`, "the submitted key comes back")
			}
		})
	}
}

func TestBidFormDatabaseFailureFallsBackToErrorPage(t *testing.T) {
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) { return lot.PublicLot{}, errLotsDown },
		placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
			return lot.PlacedBid{}, errLotsDown
		},
	}
	server, participant := participantClient(t, lots)
	token := bidAPIToken(t, participant, server.URL)

	resp := postFormWithHeaders(t, participant, server.URL+"/lots/7/bids",
		url.Values{"amount": {"101"}, "request_key": {validBidKey}, "gorilla.csrf.Token": {token}}, nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, resp.Body, "временно недоступен")
	assert.NotContains(t, resp.Body, "fake lots down")
}

func TestBidFormAccessControl(t *testing.T) {
	lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return placedBid(false), nil
	}}
	server, guest := newTestServer(t, &fakeAuthenticator{}, testConfig(), lots)
	token := bidAPIToken(t, guest, server.URL)

	resp := postFormWithHeaders(t, guest, server.URL+"/lots/7/bids",
		url.Values{"amount": {"101"}, "request_key": {validBidKey}, "gorilla.csrf.Token": {token}}, nil)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "a guest is sent to the login page, not errored")
	assert.Equal(t, "/login", resp.Header.Get("Location"))
	assert.Zero(t, lots.placeCalls)

	server, admin := adminClient(t, lots)
	token = bidAPIToken(t, admin, server.URL)
	resp = postFormWithHeaders(t, admin, server.URL+"/lots/7/bids",
		url.Values{"amount": {"101"}, "request_key": {validBidKey}, "gorilla.csrf.Token": {token}}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "an administrator must not bid")
	assert.Zero(t, lots.placeCalls)

	server, participant := participantClient(t, lots)
	resp = postFormWithHeaders(t, participant, server.URL+"/lots/7/bids",
		url.Values{"amount": {"101"}, "request_key": {validBidKey}}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the form post carries no CSRF token")
	assert.Zero(t, lots.placeCalls)

	// A missing lot is a plain 404 for the form as well; the token must come
	// from this client's own CSRF cookie.
	token = bidAPIToken(t, participant, server.URL)
	lots.publicFunc = func(context.Context, int64) (lot.PublicLot, error) { return lot.PublicLot{}, lot.ErrNotFound }
	lots.placeFunc = func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
		return lot.PlacedBid{}, lot.ErrNotFound
	}
	resp = postFormWithHeaders(t, participant, server.URL+"/lots/987654/bids",
		url.Values{"amount": {"101"}, "request_key": {validBidKey}, "gorilla.csrf.Token": {token}}, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestBidFormAndAPIShareOneOperation(t *testing.T) {
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) { return activeBidLot(), nil },
		placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
			return placedBid(false), nil
		},
	}
	server, participant, key, token := bidFormSetup(t, lots)

	resp := postBidForm(t, participant, server.URL+"/lots/7/bids", "101", key, token)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	formCall := lots.lastPlace
	require.Equal(t, 1, lots.placeCalls)

	// The API post carries the token of the /-anchored cookie, exactly like
	// the browser's JavaScript does after a login.
	apiToken := bidAPIToken(t, participant, server.URL)
	resp = postJSON(t, participant, server.URL+"/api/lots/7/bids", bidAPIBody("202", validBidKey),
		map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	assert.Equal(t, formCall.ParticipantID, lots.lastPlace.ParticipantID,
		"both paths resolve the participant from the same session")
	assert.Equal(t, int64(7), lots.lastPlace.LotID)
	assert.Equal(t, int64(202), lots.lastPlace.Amount)
	assert.NotEqual(t, formCall.RequestKey, lots.lastPlace.RequestKey, "the API call carries its own request key")
}
