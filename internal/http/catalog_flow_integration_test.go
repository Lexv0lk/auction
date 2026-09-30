//go:build integration

package httpapp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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

// TestCatalogFlowAgainstPostgreSQL walks the participant catalog and lot page
// through the real services and PostgreSQL. The auction outcomes (bids,
// finished results, an overdue deadline) come from isolated SQL fixtures: the
// bid operation itself is the subject of steps 09-11.
func TestCatalogFlowAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	hash, err := password.Hash("integration-pass")
	require.NoError(t, err)
	var adminID, participantID int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ('flow-catalog-admin', $1, 'admin') RETURNING id", hash).
		Scan(&adminID))
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ('flow-catalog-participant', $1, 'participant') RETURNING id", hash).
		Scan(&participantID))

	var coinCategoryID, bookCategoryID int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ('Нумизматика') RETURNING id").
		Scan(&coinCategoryID))
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ('Антикварные книги') RETURNING id").
		Scan(&bookCategoryID))

	lotService := lot.NewService(pool)
	endsAt := time.Now().Add(48 * time.Hour)

	// The running auction with eleven bids: the history spans two pages.
	activeLot := publishDraft(t, ctx, lotService, lot.Input{
		Title: "Серебряный рубль 1726 года", Description: "Монета в хорошем состоянии",
		CategoryID: coinCategoryID, StartPrice: 5000, EndsAt: endsAt,
	})
	insertBid(t, ctx, pool, activeLot, participantID, 5000, 11)
	for i := 1; i <= 10; i++ {
		insertBid(t, ctx, pool, activeLot, participantID, int64(5000+i), int64(11-i))
	}

	// The draft is invisible to the catalog, the page and the API.
	draftLot := createDraft(t, ctx, lotService, lot.Input{
		Title: "Скрытый черновик", Description: "<b>Разметка</b> описания",
		CategoryID: bookCategoryID, StartPrice: 700, EndsAt: endsAt,
	})

	// A finished lot with a recorded winner.
	finishedLot := publishDraft(t, ctx, lotService, lot.Input{
		Title: "Земская марка 1889 года", Description: "Марка без клея",
		CategoryID: bookCategoryID, StartPrice: 1200, EndsAt: endsAt,
	})
	winningBidID := insertBid(t, ctx, pool, finishedLot, participantID, 1500, 100)
	finishLot(t, ctx, pool, finishedLot, &winningBidID)

	// A finished lot without any bid and therefore without a winner.
	emptyFinishedLot := publishDraft(t, ctx, lotService, lot.Input{
		Title: "Каталог выставки 1903 года", Description: "Переплёт потёрт",
		CategoryID: bookCategoryID, StartPrice: 3000, EndsAt: endsAt,
	})
	finishLot(t, ctx, pool, emptyFinishedLot, nil)

	// An active lot whose deadline has passed: the result is being determined.
	overdueLot := publishDraft(t, ctx, lotService, lot.Input{
		Title: "Акварель с видом на залив", Description: "Без рамы",
		CategoryID: coinCategoryID, StartPrice: 2500, EndsAt: time.Now().Add(time.Hour),
	})
	_, err = pool.Exec(ctx, "UPDATE lots SET ends_at = now() - interval '1 minute' WHERE id = $1", overdueLot)
	require.NoError(t, err)
	// A running auction whose price reached the int64 maximum: no next bid is
	// computable and every digit must survive.
	maxPriceLot := publishDraft(t, ctx, lotService, lot.Input{
		Title: "Максимальная цена", Description: "Проверка больших чисел",
		CategoryID: coinCategoryID, StartPrice: 1, EndsAt: endsAt,
	})
	insertBid(t, ctx, pool, maxPriceLot, participantID, math.MaxInt64, 50)

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), lotService, nil, testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	guest, err := cookiejar.New(nil)
	require.NoError(t, err)
	guestClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           guest,
	}
	resp := getRequest(t, guestClient, server.URL+"/lots")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "a guest is redirected to the login")
	resp = getRequest(t, guestClient, server.URL+"/api/lots/"+strconv.FormatInt(activeLot, 10))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	participant := loginClient(t, server, "flow-catalog-participant", "integration-pass")

	// The catalog shows every published lot and never the draft. Running
	// auctions come first, the result determination next, finished lots last.
	resp = getRequest(t, participant, server.URL+"/lots")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assertOrder(t, resp.Body, "Серебряный рубль 1726 года", "Акварель с видом на залив", "Земская марка 1889 года")
	assert.Contains(t, resp.Body, "Каталог выставки 1903 года")
	assert.Contains(t, resp.Body, ">5010<", "the current price is the maximal accepted bid")
	assert.NotContains(t, resp.Body, "Скрытый черновик", "drafts never appear in the catalog")
	assert.NotContains(t, resp.Body, "&lt;b&gt;Разметка&lt;/b&gt;")

	// Filters: category and display state.
	resp = getRequest(t, participant, server.URL+"/lots?category="+strconv.FormatInt(coinCategoryID, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Серебряный рубль 1726 года")
	assert.NotContains(t, resp.Body, "Земская марка 1889 года", "the category filter narrows the list")

	resp = getRequest(t, participant, server.URL+"/lots?state=active")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Серебряный рубль 1726 года")
	assert.NotContains(t, resp.Body, "Акварель с видом на залив", "an overdue lot is not \"active\"")

	resp = getRequest(t, participant, server.URL+"/lots?state=determining")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Акварель с видом на залив")
	assert.NotContains(t, resp.Body, "Серебряный рубль 1726 года")

	resp = getRequest(t, participant, server.URL+"/lots?state=finished")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Земская марка 1889 года")
	assert.NotContains(t, resp.Body, "Серебряный рубль 1726 года")

	// Direct access to a draft is a 404 for the page and for the API.
	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(draftLot, 10))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = getRequest(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(draftLot, 10))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Body, "lot_not_found")

	// The lot page: current price, minimum next bid, history pagination.
	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Монета в хорошем состоянии")
	assert.Contains(t, resp.Body, ">5010<", "current price")
	assert.Contains(t, resp.Body, ">5011<", "minimum next bid")
	assert.Equal(t, lot.BidHistoryPageSize, strings.Count(resp.Body, "<tr><td>"), "the first history page holds exactly the page size")
	assert.Contains(t, resp.Body, "Следующая страница")

	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(activeLot, 10)+"?bids=2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, strings.Count(resp.Body, "<tr><td>"), "the second history page holds the single oldest bid")
	assert.Contains(t, resp.Body, "Предыдущая страница")

	// The overdue active lot: waiting for the result, no bid form.
	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(overdueLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Торги завершены, определяется результат")
	assert.NotContains(t, resp.Body, `id="bid-form"`)

	// The finished lots: with a winner and without one.
	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(finishedLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Победитель: <strong>flow-catalog-participant</strong>")
	assert.Contains(t, resp.Body, "сумма ставки 1500")

	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(emptyFinishedLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "победитель не определён")

	// The maximal price: every digit is served, and no next bid is offered.
	resp = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(maxPriceLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "9223372036854775807")
	assert.Contains(t, resp.Body, "предел цены достигнут")

	// The state API mirrors the page state machine.
	resp = getRequest(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(overdueLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var determiningBody struct {
		DisplayStatus  string  `json:"display_status"`
		Status         string  `json:"status"`
		MinimumNextBid *string `json:"minimum_next_bid"`
		CanBid         bool    `json:"can_bid"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &determiningBody))
	assert.Equal(t, "determining", determiningBody.DisplayStatus)
	assert.Equal(t, "active", determiningBody.Status, "the stored status is unchanged until the worker finishes the lot")
	assert.Nil(t, determiningBody.MinimumNextBid)
	assert.False(t, determiningBody.CanBid)

	resp = getRequest(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(finishedLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var finishedBody struct {
		DisplayStatus string `json:"display_status"`
		WinningBid    *struct {
			ID          string `json:"id"`
			Amount      string `json:"amount"`
			Participant string `json:"participant"`
		} `json:"winning_bid"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &finishedBody))
	assert.Equal(t, "finished", finishedBody.DisplayStatus)
	require.NotNil(t, finishedBody.WinningBid)
	assert.Equal(t, "1500", finishedBody.WinningBid.Amount)
	assert.Equal(t, "flow-catalog-participant", finishedBody.WinningBid.Participant)
	assert.Equal(t, strconv.FormatInt(winningBidID, 10), finishedBody.WinningBid.ID)

	resp = getRequest(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(maxPriceLot, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `"current_price":"9223372036854775807"`, "the string carries every digit")
	assert.Contains(t, resp.Body, `"minimum_next_bid":null`)
	assert.Contains(t, resp.Body, `"can_bid":false`)

	// The escaped draft description proves the draft row itself was stored;
	// its invisibility above is the visibility rule, not missing data.
	var storedDescription string
	require.NoError(t, pool.QueryRow(ctx, "SELECT description FROM lots WHERE id = $1", draftLot).Scan(&storedDescription))
	assert.Equal(t, "<b>Разметка</b> описания", storedDescription)
}

func createDraft(t *testing.T, ctx context.Context, lotService *lot.Service, input lot.Input) int64 {
	t.Helper()

	created, err := lotService.Create(ctx, input)
	require.NoError(t, err)

	return created.ID
}

func publishDraft(t *testing.T, ctx context.Context, lotService *lot.Service, input lot.Input) int64 {
	t.Helper()

	created, err := lotService.Create(ctx, input)
	require.NoError(t, err)
	_, err = lotService.Publish(ctx, created.ID)
	require.NoError(t, err)

	return created.ID
}

// insertBid writes an accepted bid fixture and returns its ID. The request
// keys are unique per lot and amount, the accepted times are spread to pin
// the history order.
func insertBid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID, userID, amount int64, minutesAgo int64) int64 {
	t.Helper()

	var bidID int64
	err := pool.QueryRow(ctx,
		"INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key)"+
			" VALUES ($1, $2, $3, now() - make_interval(mins => $4), $5::uuid) RETURNING id",
		lotID, userID, amount, minutesAgo, fmt.Sprintf("00000000-0000-4000-8000-%012x", uint64(amount)&0xffffffffffff)). //nolint:gosec // the amount is a positive fixture, the mask keeps the key bounded
		Scan(&bidID)
	require.NoError(t, err)

	return bidID
}

// finishLot records the final state of a fixture lot the way the worker of
// step 11 will: a winner references an existing bid, a winnerless lot only
// carries finished_at.
func finishLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64, winningBidID *int64) {
	t.Helper()

	if winningBidID != nil {
		_, err := pool.Exec(ctx,
			"UPDATE lots SET status = 'finished', finished_at = now(), winning_bid_id = $2 WHERE id = $1",
			lotID, *winningBidID)
		require.NoError(t, err)

		return
	}

	_, err := pool.Exec(ctx,
		"UPDATE lots SET status = 'finished', finished_at = now() WHERE id = $1", lotID)
	require.NoError(t, err)
}

// assertOrder checks that the names appear in the body in the given order.
func assertOrder(t *testing.T, body string, names ...string) {
	t.Helper()

	positions := make([]int, 0, len(names))
	for _, name := range names {
		position := strings.Index(body, name)
		require.NotEqual(t, -1, position, "%q must be on the page", name)
		if len(positions) > 0 {
			assert.Greater(t, position, positions[len(positions)-1], "%q must follow the previous name", name)
		}
		positions = append(positions, position)
	}
}
