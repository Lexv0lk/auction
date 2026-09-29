//go:build integration

package httpapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/testutil"
	"github.com/Lexv0lk/auction/internal/worker"
)

// gatedPool delays the worker's transactions until the test opens the gate.
// The barrier exists only in this test file: the application itself has no
// profile or switch to hold the worker back.
type gatedPool struct {
	*pgxpool.Pool
	gate chan struct{}
}

func (p *gatedPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	select {
	case <-p.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return p.Pool.BeginTx(ctx, options)
}

// TestWorkerCompletesAuctionBehindBarrier runs the real HTTP handler and the
// real background worker over one PostgreSQL: after the deadline the HTTP
// side already refuses bids while the barrier holds the pass back, and once
// the barrier opens the recorded result appears on its own through the state
// API and the page.
func TestWorkerCompletesAuctionBehindBarrier(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Нумизматика worker flow")
	participantID := createFlowBidUser(t, ctx, pool, "flow-worker-participant", auth.RoleParticipant, "integration-pass")

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), lot.NewService(pool), nil, testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// The deadline has already passed: the participants must be refused
	// right now, while the recorded result does not exist yet.
	lotID := createFlowLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Second))

	gate := make(chan struct{})
	background := worker.New(&gatedPool{Pool: pool, gate: gate}, discardLogger(), nil)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	t.Cleanup(cancelWorker)
	go func() {
		_ = background.Run(workerCtx, config.Worker{PollInterval: 20 * time.Millisecond, BatchSize: 10})
	}()

	participant := loginClient(t, server, "flow-worker-participant", "integration-pass")
	apiToken := bidAPIToken(t, participant, server.URL)

	// After the deadline the bid is refused although the worker has not run:
	// the deadline decides, not the recorded status.
	resp := postJSON(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(lotID, 10)+"/bids",
		bidAPIBody("150", validBidKey), map[string]string{"X-CSRF-Token": apiToken})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "auction_closed")
	assert.Zero(t, bidRowCount(t, ctx, pool, lotID))

	// The page shows the determination state while the pass is held back.
	page := getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(lotID, 10))
	assert.Contains(t, page.Body, "определяется результат")

	// A bid committed before the deadline (as the seed of a race would be)
	// becomes the winner the worker records.
	winnerID := insertFlowBid(t, ctx, pool, lotID, participantID, 150)

	close(gate)

	state := waitFlowLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, winnerID, *state.winningBidID, "the recorded winner is the maximal committed bid")

	resp = getRequest(t, participant, server.URL+"/api/lots/"+strconv.FormatInt(lotID, 10))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var stateBody struct {
		DisplayStatus string `json:"display_status"`
		CanBid        bool   `json:"can_bid"`
		WinningBid    *struct {
			ID          string `json:"id"`
			Amount      string `json:"amount"`
			Participant string `json:"participant"`
		} `json:"winning_bid"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &stateBody))
	assert.Equal(t, "finished", stateBody.DisplayStatus)
	assert.False(t, stateBody.CanBid)
	require.NotNil(t, stateBody.WinningBid, "the state API answers the recorded winner")
	assert.Equal(t, "150", stateBody.WinningBid.Amount)
	assert.Equal(t, "flow-worker-participant", stateBody.WinningBid.Participant)

	// The page renders the server-recorded result; the browser never
	// computes the winner itself.
	page = getRequest(t, participant, server.URL+"/lots/"+strconv.FormatInt(lotID, 10))
	assert.Contains(t, page.Body, "Победитель")
	assert.Contains(t, page.Body, ">150<")
}

// insertFlowBid writes an accepted bid directly, imitating a bid committed
// before the deadline.
func insertFlowBid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID, userID, amount int64) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		lotID, userID, amount, time.Now().Add(-time.Minute),
		"aaa00000-0000-4000-8000-000000000001").Scan(&id))

	return id
}

// flowLotState is the stored result of a lot as the flow tests read it back.
type flowLotState struct {
	status       string
	finishedAt   *time.Time
	winningBidID *int64
}

func readFlowLotState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) flowLotState {
	t.Helper()

	var state flowLotState
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status, finished_at, winning_bid_id FROM lots WHERE id = $1", lotID).
		Scan(&state.status, &state.finishedAt, &state.winningBidID))

	return state
}

// waitFlowLotFinished polls the stored state until the worker has recorded
// the finish, bounded by the test deadline.
func waitFlowLotFinished(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) flowLotState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		state := readFlowLotState(t, ctx, pool, lotID)
		if state.status == lot.StatusFinished {
			return state
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, "the lot was never finished", "lot %d stayed %s", lotID, state.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
