package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
)

var errLotsDown = errors.New("lots database down")

func participantClient(t *testing.T, deps ...any) (*httptest.Server, *http.Client) {
	t.Helper()

	user := auth.User{ID: 2, Login: "demo-participant-1", Role: auth.RoleParticipant}

	return loggedInClient(t, user, deps...)
}

func TestCatalogRequiresLogin(t *testing.T) {
	server, client := newTestServer(t, &fakeAuthenticator{}, testConfig())

	resp := getRequest(t, client, server.URL+"/lots")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	resp = getRequest(t, client, server.URL+"/lots/7")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp = getRequest(t, client, server.URL+"/api/lots/7")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the API answers guests with JSON 401")
}

func TestCatalogPassesFiltersAndShowsItems(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	lots := &fakeLots{
		catalogFunc: func(_ context.Context, _ lot.CatalogFilter) ([]lot.CatalogItem, bool, error) {
			return []lot.CatalogItem{
				{ID: 7, Title: "Серебряный рубль", CategoryID: 3, CategoryName: "Нумизматика",
					StartPrice: 5000, CurrentPrice: 6000, State: lot.DisplayStateActive, EndsAt: endsAt},
				{ID: 8, Title: "Марка", CategoryID: 1, CategoryName: "Филателия",
					StartPrice: 1200, CurrentPrice: 1200, State: lot.DisplayStateDetermining, EndsAt: endsAt},
			}, true, nil
		},
	}
	categories := &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return []category.Category{{ID: 3, Name: "Нумизматика"}}, nil
		},
	}
	server, client := participantClient(t, categories, lots)

	resp := getRequest(t, client, server.URL+"/lots?category=3&state=active&page=2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, lots.catalogCalls)
	assert.Equal(t, lot.CatalogFilter{CategoryID: 3, State: "active", Page: 2}, lots.lastFilter)

	assert.Contains(t, resp.Body, `/lots/7">Серебряный рубль</a>`, "every item links its lot page")
	assert.Contains(t, resp.Body, "Нумизматика")
	assert.Contains(t, resp.Body, ">6000<", "the current price is shown, not only the start price")
	assert.Contains(t, resp.Body, "торги идут")
	assert.Contains(t, resp.Body, "торги завершены, определяется результат")
	assert.Contains(t, resp.Body, "Следующая страница", "hasMore turns into a next-page link")
	assert.Contains(t, resp.Body, "Предыдущая страница", "page 2 links back")

	// A malformed filter is a read, not an error: the values fall back to the
	// defaults.
	resp = getRequest(t, client, server.URL+"/lots?category=abc&state=hack&page=-4")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, lot.CatalogFilter{Page: 1}, lots.lastFilter)
	assert.NotContains(t, resp.Body, "Предыдущая страница", "the first page has no previous page")
}

func TestCatalogServiceFailureIs503(t *testing.T) {
	lots := &fakeLots{catalogFunc: func(context.Context, lot.CatalogFilter) ([]lot.CatalogItem, bool, error) {
		return nil, false, errLotsDown
	}}
	server, client := participantClient(t, lots)

	resp := getRequest(t, client, server.URL+"/lots")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestLotPageShowsStatesAndResults(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	winningID := int64(42)

	cases := []struct {
		name          string
		public        lot.PublicLot
		wantContains  []string
		wantNotExpect []string
	}{
		{
			name: "active before the first bid",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 5000, Status: lot.StatusActive, State: lot.DisplayStateActive,
				CurrentPrice: 5000, EndsAt: endsAt},
			wantContains:  []string{"5000", "Ставок ещё не было", `name="request_key"`, `id="bid-form"`, "Сделать ставку"},
			wantNotExpect: []string{"Победитель"},
		},
		{
			name: "active with bids",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 5000, Status: lot.StatusActive, State: lot.DisplayStateActive,
				CurrentPrice: 6000, MaxBid: 6000, EndsAt: endsAt},
			wantContains: []string{">6000<", ">6001<", "История ставок"},
		},
		{
			name: "overdue active lot determines the result",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 5000, Status: lot.StatusActive, State: lot.DisplayStateDetermining,
				CurrentPrice: 6000, MaxBid: 6000, EndsAt: endsAt},
			wantContains:  []string{"Торги завершены, определяется результат"},
			wantNotExpect: []string{`id="bid-form"`},
		},
		{
			name: "finished with a winner",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 5000, Status: lot.StatusFinished, State: lot.DisplayStateFinished,
				CurrentPrice: 6000, MaxBid: 6000, WinningBidID: &winningID,
				WinningAmount: 6000, WinningParticipant: "demo-participant-2", EndsAt: endsAt},
			wantContains:  []string{"Победитель: <strong>demo-participant-2</strong>", "сумма ставки 6000"},
			wantNotExpect: []string{`id="bid-form"`},
		},
		{
			name: "finished without a winner",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 5000, Status: lot.StatusFinished, State: lot.DisplayStateFinished,
				CurrentPrice: 5000, EndsAt: endsAt},
			wantContains:  []string{"победитель не определён"},
			wantNotExpect: []string{`id="bid-form"`},
		},
		{
			name: "maximal int64 price offers no next bid",
			public: lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание", CategoryName: "Нумизматика",
				StartPrice: 1, Status: lot.StatusActive, State: lot.DisplayStateActive,
				CurrentPrice: math.MaxInt64, MaxBid: math.MaxInt64, EndsAt: endsAt},
			wantContains: []string{"9223372036854775807", "предел цены достигнут"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lots := &fakeLots{
				publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
					return testCase.public, nil
				},
			}
			server, client := participantClient(t, lots)

			resp := getRequest(t, client, server.URL+"/lots/7")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			for _, wanted := range testCase.wantContains {
				assert.Contains(t, resp.Body, wanted)
			}
			for _, unexpected := range testCase.wantNotExpect {
				assert.NotContains(t, resp.Body, unexpected)
			}
		})
	}
}

func TestLotPageEscapesDescriptionAndShowsBids(t *testing.T) {
	acceptedAt := time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
			return lot.PublicLot{ID: 7, Title: "Рубль", Description: "<b>Жирное</b> описание",
				CategoryName: "Нумизматика", StartPrice: 5000, Status: lot.StatusActive,
				State: lot.DisplayStateActive, CurrentPrice: 6000, MaxBid: 6000, EndsAt: acceptedAt.Add(time.Hour),
				DatabaseNow: acceptedAt}, nil
		},
		bidsFunc: func(_ context.Context, lotID int64, page int) ([]lot.Bid, bool, error) {
			require.Equal(t, int64(7), lotID)
			require.Equal(t, 2, page, "the bids page parameter reaches the service")

			return []lot.Bid{
				{ID: 9, Participant: "demo-participant-1", Amount: 6000, AcceptedAt: acceptedAt},
			}, false, nil
		},
	}
	server, client := participantClient(t, lots)

	resp := getRequest(t, client, server.URL+"/lots/7?bids=2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "&lt;b&gt;Жирное&lt;/b&gt;")
	assert.NotContains(t, resp.Body, "<b>Жирное</b>")
	assert.Contains(t, resp.Body, "demo-participant-1")
	assert.Contains(t, resp.Body, ">6000<")
	assert.Contains(t, resp.Body, "Предыдущая страница", "a later bids page links back")
	assert.NotContains(t, resp.Body, "Следующая страница")
}

func TestLotPageHidesMissingAndDraftLots(t *testing.T) {
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
			return lot.PublicLot{}, lot.ErrNotFound
		},
	}
	server, client := participantClient(t, lots)

	resp := getRequest(t, client, server.URL+"/lots/987654")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = getRequest(t, client, server.URL+"/lots/not-a-number")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLotStateAPIContract(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	dbNow := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	acceptedAt := time.Date(2026, 9, 29, 11, 30, 0, 0, time.UTC)
	winningID := int64(42)
	lots := &fakeLots{
		publicFunc: func(_ context.Context, id int64) (lot.PublicLot, error) {
			require.Equal(t, int64(7), id)

			return lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание",
				CategoryID: 3, CategoryName: "Нумизматика", StartPrice: 5000,
				Status: lot.StatusFinished, State: lot.DisplayStateFinished, EndsAt: endsAt,
				DatabaseNow: dbNow, CurrentPrice: 6000, MaxBid: 6000,
				WinningBidID: &winningID, WinningAmount: 6000, WinningParticipant: "demo-participant-2"}, nil
		},
		bidsFunc: func(context.Context, int64, int) ([]lot.Bid, bool, error) {
			return []lot.Bid{
				{ID: 42, Participant: "demo-participant-2", Amount: 6000, AcceptedAt: acceptedAt},
			}, true, nil
		},
	}
	server, client := participantClient(t, lots)

	resp := getRequest(t, client, server.URL+"/api/lots/7")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var body struct {
		ID             string         `json:"id"`
		Title          string         `json:"title"`
		Category       map[string]any `json:"category"`
		Status         string         `json:"status"`
		DisplayStatus  string         `json:"display_status"`
		ServerTime     string         `json:"server_time"`
		EndsAt         string         `json:"ends_at"`
		StartPrice     string         `json:"start_price"`
		CurrentPrice   string         `json:"current_price"`
		MinimumNextBid *string        `json:"minimum_next_bid"`
		CanBid         bool           `json:"can_bid"`
		WinningBid     *struct {
			ID          string `json:"id"`
			Amount      string `json:"amount"`
			Participant string `json:"participant"`
		} `json:"winning_bid"`
		BidHistory []map[string]any `json:"bid_history"`
		HasMore    bool             `json:"bid_history_has_more"`
		NextPage   *int             `json:"bid_history_next_page"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))

	assert.Equal(t, "7", body.ID, "identifiers are decimal strings")
	assert.Equal(t, "Рубль", body.Title)
	assert.Equal(t, "3", body.Category["id"])
	assert.Equal(t, "Нумизматика", body.Category["name"])
	assert.Equal(t, "finished", body.Status)
	assert.Equal(t, "finished", body.DisplayStatus)
	assert.Equal(t, "5000", body.StartPrice, "money values are decimal strings")
	assert.Equal(t, "6000", body.CurrentPrice)
	assert.Nil(t, body.MinimumNextBid, "a finished lot offers no next bid")
	assert.False(t, body.CanBid)
	require.NotNil(t, body.WinningBid)
	assert.Equal(t, "42", body.WinningBid.ID)
	assert.Equal(t, "6000", body.WinningBid.Amount)
	assert.Equal(t, "demo-participant-2", body.WinningBid.Participant)
	require.Len(t, body.BidHistory, 1)
	assert.Equal(t, "6000", body.BidHistory[0]["amount"])
	assert.NotContains(t, body.BidHistory[0], "request_key", "request keys never leave the database")
	assert.True(t, body.HasMore)
	require.NotNil(t, body.NextPage)
	assert.Equal(t, 2, *body.NextPage)

	parsedTime, err := time.Parse(time.RFC3339, body.ServerTime)
	require.NoError(t, err)
	assert.True(t, dbNow.Equal(parsedTime), "server_time is the database clock, RFC 3339")
	parsedEndsAt, err := time.Parse(time.RFC3339, body.EndsAt)
	require.NoError(t, err)
	assert.True(t, endsAt.Equal(parsedEndsAt))
}

func TestLotStateAPIActiveAndMissing(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	dbNow := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
			return lot.PublicLot{ID: 7, Title: "Рубль", Description: "Описание",
				CategoryID: 3, CategoryName: "Нумизматика", StartPrice: 5000,
				Status: lot.StatusActive, State: lot.DisplayStateActive, EndsAt: endsAt,
				DatabaseNow: dbNow, CurrentPrice: 5000, MaxBid: 0}, nil
		},
	}
	server, client := participantClient(t, lots)

	resp := getRequest(t, client, server.URL+"/api/lots/7")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		DisplayStatus  string  `json:"display_status"`
		MinimumNextBid *string `json:"minimum_next_bid"`
		CanBid         bool    `json:"can_bid"`
		WinningBid     *string `json:"winning_bid"`
		BidHistory     []any   `json:"bid_history"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &body))
	assert.Equal(t, "active", body.DisplayStatus)
	require.NotNil(t, body.MinimumNextBid)
	assert.Equal(t, "5000", *body.MinimumNextBid, "before the first bid the minimum next bid is the start price")
	assert.True(t, body.CanBid)
	assert.Nil(t, body.WinningBid)
	assert.Empty(t, body.BidHistory)

	// A draft and a missing lot answer the same JSON 404.
	lots.publicFunc = func(context.Context, int64) (lot.PublicLot, error) {
		return lot.PublicLot{}, lot.ErrNotFound
	}
	resp = getRequest(t, client, server.URL+"/api/lots/7")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Body, `"lot_not_found"`)

	lots.publicFunc = func(context.Context, int64) (lot.PublicLot, error) {
		return lot.PublicLot{}, errLotsDown
	}
	resp = getRequest(t, client, server.URL+"/api/lots/7")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
