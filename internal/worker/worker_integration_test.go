//go:build integration

package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/testutil"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return ctx
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func createWorkerUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, login string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ($1, 'integration-hash', 'participant') RETURNING id",
		login).Scan(&id))

	return id
}

func createWorkerCategory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ($1) RETURNING id", name).Scan(&id))

	return id
}

// createWorkerLot inserts a lot in the requested state directly, with any
// deadline; finished lots get a finished_at to satisfy the result constraint.
func createWorkerLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, categoryID int64, status string, startPrice int64, endsAt time.Time) int64 {
	t.Helper()

	finishedAt := "NULL"
	if status == lot.StatusFinished {
		finishedAt = "now()"
	}
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at, finished_at)"+
			" VALUES ('Лот фонового завершения', 'Описание лота фонового завершения', $1, $2, $3, $4, "+finishedAt+") RETURNING id",
		categoryID, startPrice, status, endsAt).Scan(&id))

	return id
}

var workerBidKeySeq int

// insertWorkerBid writes an accepted bid directly, imitating a bid committed
// before the deadline; the request keys stay unique.
func insertWorkerBid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID, userID, amount int64, acceptedAt time.Time) int64 {
	t.Helper()
	workerBidKeySeq++

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		lotID, userID, amount, acceptedAt,
		fmt.Sprintf("%08d-0000-4000-8000-%012d", workerBidKeySeq, workerBidKeySeq)).Scan(&id))

	return id
}

// lotRowState is the stored result of a lot as the tests read it back.
type lotRowState struct {
	status       string
	finishedAt   *time.Time
	winningBidID *int64
}

func readLotRowState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) lotRowState {
	t.Helper()

	var state lotRowState
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status, finished_at, winning_bid_id FROM lots WHERE id = $1", lotID).
		Scan(&state.status, &state.finishedAt, &state.winningBidID))

	return state
}

func waitFinished(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) lotRowState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		state := readLotRowState(t, ctx, pool, lotID)
		if state.status == lot.StatusFinished {
			return state
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, "the lot was never finished", "lot %d stayed %s", lotID, state.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFinishLotRecordsMaxAcceptedBid(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Нумизматика worker")
	firstBidder := createWorkerUser(t, ctx, pool, "worker-bidder-1")
	secondBidder := createWorkerUser(t, ctx, pool, "worker-bidder-2")
	endsAt := time.Now().Add(-time.Minute)
	lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, endsAt)
	lowBid := insertWorkerBid(t, ctx, pool, lotID, firstBidder, 100, endsAt.Add(-time.Minute))
	highBid := insertWorkerBid(t, ctx, pool, lotID, secondBidder, 150, endsAt.Add(-30*time.Second))

	outcome, found, err := w.finishLot(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, lotID, outcome.LotID)
	require.NotNil(t, outcome.WinningBidID)
	assert.Equal(t, highBid, *outcome.WinningBidID, "the maximal accepted bid wins, not the latest")
	assert.Equal(t, int64(150), outcome.WinningAmount)
	assert.False(t, outcome.FinishedAt.Before(endsAt), "finished_at is recorded after the deadline")
	assert.GreaterOrEqual(t, outcome.Delay(), time.Minute, "the delay spans from the deadline to the recorded finish")

	state := readLotRowState(t, ctx, pool, lotID)
	assert.Equal(t, lot.StatusFinished, state.status)
	require.NotNil(t, state.finishedAt)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, highBid, *state.winningBidID)
	assert.False(t, lowBid == 0, "sanity: both bids stay stored")
}

func TestFinishLotStoresNullWinnerWithoutBids(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Филателия worker")
	endsAt := time.Now().Add(-time.Minute)
	lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, endsAt)

	outcome, found, err := w.finishLot(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, lotID, outcome.LotID)
	assert.Nil(t, outcome.WinningBidID)
	assert.Zero(t, outcome.WinningAmount)

	state := readLotRowState(t, ctx, pool, lotID)
	assert.Equal(t, lot.StatusFinished, state.status)
	require.NotNil(t, state.finishedAt)
	assert.Nil(t, state.winningBidID, "a lot without bids finishes without a winner")
}

func TestFinishLotLeavesNonDueLotsUntouched(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Антикварные книги worker")

	futureID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))
	draftID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusDraft, 100, time.Now().Add(-time.Hour))
	finishedID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusFinished, 100, time.Now().Add(-time.Hour))
	before := map[int64]lotRowState{
		futureID:   readLotRowState(t, ctx, pool, futureID),
		draftID:    readLotRowState(t, ctx, pool, draftID),
		finishedID: readLotRowState(t, ctx, pool, finishedID),
	}

	_, found, err := w.finishLot(ctx)
	require.NoError(t, err)
	assert.False(t, found, "an active lot with a future deadline is not due")

	assert.Equal(t, before, map[int64]lotRowState{
		futureID:   readLotRowState(t, ctx, pool, futureID),
		draftID:    readLotRowState(t, ctx, pool, draftID),
		finishedID: readLotRowState(t, ctx, pool, finishedID),
	}, "a draft, a running lot and an already finished lot are never touched")
}

func TestPassFinishesExactlyDueLots(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Живопись worker")

	overdueFirst := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-2*time.Minute))
	overdueSecond := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
	futureID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))

	finished, err := w.pass(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(2), finished)

	assert.Equal(t, lot.StatusFinished, readLotRowState(t, ctx, pool, overdueFirst).status)
	assert.Equal(t, lot.StatusFinished, readLotRowState(t, ctx, pool, overdueSecond).status)
	assert.Equal(t, lot.StatusActive, readLotRowState(t, ctx, pool, futureID).status,
		"the running auction with a future deadline is never finished early")
}

func TestPassRespectsBatchSize(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Нумизматика worker")

	var due []int64
	for i := 0; i < 3; i++ {
		due = append(due, createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute)))
	}

	finished, err := w.pass(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), finished, "one pass never processes more than its batch size")
	assert.Equal(t, lot.StatusActive, readLotRowState(t, ctx, pool, due[2]).status)

	finished, err = w.pass(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(1), finished, "the next pass picks up the remaining lot")
	assert.Equal(t, lot.StatusFinished, readLotRowState(t, ctx, pool, due[2]).status)
}

func TestRunCatchesUpAfterRestart(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	categoryID := createWorkerCategory(t, ctx, pool, "Филателия worker")

	// The deadlines passed while no instance was running: a fresh process
	// must pick the accumulated lots up on its first pass.
	accumulated := []int64{
		createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Hour)),
		createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-30*time.Minute)),
	}

	runCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	w := New(pool, discardLogger())
	require.NoError(t, w.Run(runCtx, config.Worker{PollInterval: 10 * time.Millisecond, BatchSize: 10}))

	for _, lotID := range accumulated {
		assert.Equal(t, lot.StatusFinished, readLotRowState(t, ctx, pool, lotID).status,
			"the restarted worker finishes every lot accumulated during the downtime")
	}
	snapshot := w.Stats()
	assert.GreaterOrEqual(t, snapshot.FinishedLots, int64(2))
	assert.False(t, snapshot.LastSuccess.IsZero())
}

func TestConcurrentPassesFinishEachLotOnce(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	categoryID := createWorkerCategory(t, ctx, pool, "Антикварные книги worker")
	bidder := createWorkerUser(t, ctx, pool, "worker-race-bidder")

	const lots = 5
	winner := make(map[int64]int64, lots)
	for i := 0; i < lots; i++ {
		lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
		winner[lotID] = insertWorkerBid(t, ctx, pool, lotID, bidder, int64(200+i), time.Now().Add(-90*time.Second))
	}

	// Two replicas process the same due set at once: SKIP LOCKED spreads the
	// lots between them, and every lot gets exactly one recorded result.
	const replicas = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	counts := make([]int64, replicas)
	errs := make([]error, replicas)
	wg.Add(replicas)
	for i := 0; i < replicas; i++ {
		w := New(pool, discardLogger())
		go func(index int) {
			defer wg.Done()
			<-start
			counts[index], errs[index] = w.pass(ctx, 10)
		}(i)
	}
	close(start)
	wg.Wait()

	total := int64(0)
	for i, err := range errs {
		require.NoError(t, err, "replica %d must not fail on lock contention", i+1)
		total += counts[i]
	}
	assert.Equal(t, int64(lots), total, "every lot is finished exactly once across the replicas")

	for lotID, expectedWinner := range winner {
		state := waitFinished(t, ctx, pool, lotID)
		require.NotNil(t, state.winningBidID)
		assert.Equal(t, expectedWinner, *state.winningBidID, "the stored winner of lot %d is its maximal bid", lotID)
	}
}

func TestLockedLotDoesNotBlockOtherLots(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Живопись worker")
	bidder := createWorkerUser(t, ctx, pool, "worker-lock-bidder")

	held := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
	heldBid := insertWorkerBid(t, ctx, pool, held, bidder, 120, time.Now().Add(-90*time.Second))
	createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-2*time.Minute))

	// A transaction holds the lot row exactly like a pass or a bid in
	// flight: the worker must skip it and finish the other lots.
	holder, err := pool.Begin(ctx)
	require.NoError(t, err)
	var lockedID int64
	require.NoError(t, holder.QueryRow(ctx, "SELECT id FROM lots WHERE id = $1 FOR UPDATE", held).Scan(&lockedID))

	finished, err := w.pass(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), finished, "the locked lot is skipped, the other lots are finished")
	assert.Equal(t, lot.StatusActive, readLotRowState(t, ctx, pool, held).status,
		"the locked lot keeps its state while the lock is held")

	// Simulating a crash before the commit: the rollback releases the lock
	// and leaves no partial result behind.
	require.NoError(t, holder.Rollback(ctx))
	assert.Equal(t, lot.StatusActive, readLotRowState(t, ctx, pool, held).status,
		"a rolled-back completion leaves no partial result")

	finished, err = w.pass(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), finished, "the skipped lot is finished by the next pass")
	state := waitFinished(t, ctx, pool, held)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, heldBid, *state.winningBidID)
}

func TestBidAndWorkerResolveInBothOrders(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Нумизматика worker")
	bidder := createWorkerUser(t, ctx, pool, "worker-order-bidder")
	service := lot.NewService(pool)

	t.Run("bid commits before the deadline, worker counts it", func(t *testing.T) {
		endsAt := time.Now().Add(3 * time.Second)
		lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, endsAt)

		// The bid transaction holds the lot row (the fixed lock order) and
		// inserts its bid inside itself, exactly like PlaceBid: the bid is
		// not yet visible to the pass.
		holder, err := pool.Begin(ctx)
		require.NoError(t, err)
		var lockedID int64
		require.NoError(t, holder.QueryRow(ctx, "SELECT id FROM lots WHERE id = $1 FOR UPDATE", lotID).Scan(&lockedID))
		var storedBid int64
		require.NoError(t, holder.QueryRow(ctx,
			"INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key)"+
				" VALUES ($1, $2, $3, clock_timestamp(), 'bbb00000-0000-4000-8000-000000000001') RETURNING id",
			lotID, bidder, 150).Scan(&storedBid))

		finished, err := w.pass(ctx, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(0), finished, "a lot held by a bid transaction is not finished behind it")
		assert.Equal(t, lot.StatusActive, readLotRowState(t, ctx, pool, lotID).status)

		require.NoError(t, holder.Commit(ctx), "the bid commits while its instant is still before the deadline")
		var acceptedAt time.Time
		require.NoError(t, pool.QueryRow(ctx, "SELECT accepted_at FROM bids WHERE id = $1", storedBid).Scan(&acceptedAt))
		assert.True(t, acceptedAt.Before(endsAt), "the bid committed with an instant before the deadline")

		// The worker finishes nothing before the deadline: the database
		// clock decides when the result may be recorded.
		for {
			var dbNow time.Time
			require.NoError(t, pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&dbNow))
			if dbNow.After(endsAt) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		finished, err = w.pass(ctx, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(1), finished, "the pass records the result with the committed bid")
		state := readLotRowState(t, ctx, pool, lotID)
		require.NotNil(t, state.winningBidID)
		assert.Equal(t, storedBid, *state.winningBidID, "the committed bid wins")
		assert.Equal(t, lot.StatusFinished, state.status)
	})

	t.Run("worker finishes first, the bid is refused", func(t *testing.T) {
		lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))

		finished, err := w.pass(ctx, 10)
		require.NoError(t, err)
		assert.Equal(t, int64(1), finished)

		_, err = service.PlaceBid(ctx, bidder, lotID, 200, "f1000000-0000-4000-8000-000000000009")
		assert.ErrorIs(t, err, lot.ErrBiddingClosed, "a lot finished by the worker refuses every new bid")
	})
}

func TestFinishedLotIsNeverReprocessed(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Филателия worker")
	bidder := createWorkerUser(t, ctx, pool, "worker-repeat-bidder")
	lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
	insertWorkerBid(t, ctx, pool, lotID, bidder, 175, time.Now().Add(-90*time.Second))

	_, err := w.pass(ctx, 10)
	require.NoError(t, err)
	first := readLotRowState(t, ctx, pool, lotID)
	require.Equal(t, lot.StatusFinished, first.status)

	finished, err := w.pass(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(0), finished, "a finished lot is never selected again")
	second := readLotRowState(t, ctx, pool, lotID)
	require.NotNil(t, second.finishedAt)
	require.NotNil(t, first.finishedAt)
	assert.True(t, second.finishedAt.Equal(*first.finishedAt), "finished_at stays the recorded instant")
	assert.Equal(t, first.winningBidID, second.winningBidID, "the winner is never recalculated")
	assert.Equal(t, first.status, second.status)
}

func TestPassWithNoDueLotsIsSuccess(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())

	finished, err := w.pass(ctx, 10)
	require.NoError(t, err, "an empty set of due lots is a normal result")
	assert.Equal(t, int64(0), finished)
}

func TestPassReportsDatabaseFailure(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	w := New(pool, discardLogger())
	categoryID := createWorkerCategory(t, ctx, pool, "Живопись worker")
	createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))

	// The pass context is already exhausted: the operation is refused
	// instead of hanging, and the caller (the loop) keeps the failure inside
	// its bounded pause.
	passCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, err := w.pass(passCtx, 10)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the pass obeys the context budget")
}
