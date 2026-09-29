package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
)

var (
	errFakeDatabase = errors.New("fake database failure")

	dueEndsAt  = time.Date(2026, 9, 29, 11, 59, 0, 0, time.UTC)
	dbNow      = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	finishedAt = time.Date(2026, 9, 29, 12, 0, 0, 100000, time.UTC)
)

func testWorker(pool Pool) *Worker {
	return New(pool, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func discardCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return ctx
}

// dueLotTx builds the standard successful transaction: one due lot, one
// winning bid, one recorded finish.
func dueLotTx() *fakeTx {
	return &fakeTx{responses: []fakeTxResponse{
		{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
		{contains: "SELECT status, ends_at", row: fakeRow{values: []any{"active", dueEndsAt, dbNow}}},
		{contains: "SELECT id, amount FROM bids", row: fakeRow{values: []any{int64(3), int64(150)}}},
		{contains: "UPDATE lots SET", row: fakeRow{values: []any{finishedAt}}},
	}}
}

func TestFinishLotRecordsResultUnderRowLock(t *testing.T) {
	pool := &fakePool{factory: dueLotTx}
	w := testWorker(pool)

	outcome, found, err := w.finishLot(discardCtx(t))
	require.NoError(t, err)
	assert.True(t, found)
	require.NotNil(t, outcome.WinningBidID)
	assert.Equal(t, int64(7), outcome.LotID)
	assert.Equal(t, int64(3), *outcome.WinningBidID)
	assert.Equal(t, int64(150), outcome.WinningAmount)
	assert.Equal(t, finishedAt, outcome.FinishedAt)
	assert.Equal(t, dueEndsAt, outcome.EndsAt)

	require.Len(t, pool.txOptions, 1)
	assert.Equal(t, pgx.ReadCommitted, pool.txOptions[0].IsoLevel, "the finish runs in its own READ COMMITTED transaction")

	tx := pool.handedOut()[0]
	queries := tx.recordedQueries()
	require.Len(t, queries, 4)
	assert.True(t, strings.Contains(queries[0].sql, "SELECT id FROM lots"), "the due lot is selected first")
	assert.True(t, strings.Contains(queries[0].sql, "SKIP LOCKED"), "a lot locked by another replica is skipped")
	assert.True(t, strings.Contains(queries[1].sql, "SELECT status, ends_at"), "the lock holder re-verifies the lot")
	assert.Equal(t, int64(7), queries[1].args[0])
	assert.True(t, strings.Contains(queries[2].sql, "SELECT id, amount FROM bids"), "the maximal bid is read under the lot lock")
	assert.Equal(t, int64(7), queries[2].args[0])
	assert.True(t, strings.Contains(queries[3].sql, "UPDATE lots SET"), "the lot row is updated last")
	assert.Equal(t, int64(7), queries[3].args[0])
	winnerArg, ok := queries[3].args[1].(*int64)
	require.True(t, ok, "the winner argument must stay a *int64")
	require.NotNil(t, winnerArg)
	assert.Equal(t, int64(3), *winnerArg, "the update stores the winning bid")
	assert.True(t, strings.Contains(queries[3].sql, "status = 'finished'"))
	assert.True(t, tx.committed)
	// The deferred rollback is attempted after every commit; the fake records
	// both, so only the commit flag decides the outcome here.
}

func TestFinishLotWithoutBidsStoresNullWinner(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
			{contains: "SELECT status, ends_at", row: fakeRow{values: []any{"active", dueEndsAt, dbNow}}},
			{contains: "SELECT id, amount FROM bids", row: fakeRow{err: pgx.ErrNoRows}},
			{contains: "UPDATE lots SET", row: fakeRow{values: []any{finishedAt}}},
		}}
	}}
	w := testWorker(pool)

	outcome, found, err := w.finishLot(discardCtx(t))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Nil(t, outcome.WinningBidID)
	assert.Zero(t, outcome.WinningAmount)

	tx := pool.handedOut()[0]
	queries := tx.recordedQueries()
	require.Len(t, queries, 4)
	winnerArg, ok := queries[3].args[1].(*int64)
	require.True(t, ok, "the winner argument must stay a *int64")
	assert.Nil(t, winnerArg, "a lot without bids finishes with a NULL winner")
}

func TestFinishLotNoDueLotIsNormal(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: pgx.ErrNoRows}},
		}}
	}}
	w := testWorker(pool)

	_, found, err := w.finishLot(discardCtx(t))
	require.NoError(t, err)
	assert.False(t, found, "an empty selection (including a skipped locked lot) is a normal result")

	tx := pool.handedOut()[0]
	assert.Len(t, tx.recordedQueries(), 1, "nothing is read or written without a due lot")
	assert.True(t, tx.rolledBack, "the empty transaction rolls back and releases everything it holds")
	assert.False(t, tx.committed)
}

func TestFinishLotReverificationRejectsChangedLot(t *testing.T) {
	for name, verifyRow := range map[string]fakeRow{
		"finished by another replica": {values: []any{"finished", dueEndsAt, dbNow}},
		"deadline moved to future":    {values: []any{"active", dbNow.Add(time.Minute), dbNow}},
	} {
		t.Run(name, func(t *testing.T) {
			pool := &fakePool{factory: func() *fakeTx {
				return &fakeTx{responses: []fakeTxResponse{
					{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
					{contains: "SELECT status, ends_at", row: verifyRow},
				}}
			}}
			w := testWorker(pool)

			_, found, err := w.finishLot(discardCtx(t))
			require.NoError(t, err)
			assert.False(t, found, "an ineligible locked lot is left untouched")

			tx := pool.handedOut()[0]
			for _, call := range tx.recordedQueries() {
				assert.False(t, strings.Contains(call.sql, "UPDATE lots SET"), "the changed lot is never updated")
			}
			assert.True(t, tx.rolledBack, "the untouched lot is released by the rollback")
		})
	}
}

func TestFinishLotRollsBackOnDatabaseFailure(t *testing.T) {
	failures := map[string]func(*fakeTx){
		"due lot select": func(tx *fakeTx) {
			tx.responses = []fakeTxResponse{
				{contains: "SELECT id FROM lots", row: fakeRow{err: errFakeDatabase}},
			}
		},
		"re-verification": func(tx *fakeTx) {
			tx.responses = []fakeTxResponse{
				{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
				{contains: "SELECT status, ends_at", row: fakeRow{err: errFakeDatabase}},
			}
		},
		"winning bid select": func(tx *fakeTx) {
			tx.responses = []fakeTxResponse{
				{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
				{contains: "SELECT status, ends_at", row: fakeRow{values: []any{"active", dueEndsAt, dbNow}}},
				{contains: "SELECT id, amount FROM bids", row: fakeRow{err: errFakeDatabase}},
			}
		},
		"finish update": func(tx *fakeTx) {
			tx.responses = []fakeTxResponse{
				{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
				{contains: "SELECT status, ends_at", row: fakeRow{values: []any{"active", dueEndsAt, dbNow}}},
				{contains: "SELECT id, amount FROM bids", row: fakeRow{err: pgx.ErrNoRows}},
				{contains: "UPDATE lots SET", row: fakeRow{err: errFakeDatabase}},
			}
		},
	}
	for name, prepare := range failures {
		t.Run(name, func(t *testing.T) {
			tx := &fakeTx{}
			prepare(tx)
			pool := &fakePool{factory: func() *fakeTx { return tx }}
			w := testWorker(pool)

			_, found, err := w.finishLot(discardCtx(t))
			require.Error(t, err)
			assert.ErrorIs(t, err, errFakeDatabase)
			assert.False(t, found)
			assert.True(t, tx.rolledBack, "a failed pass rolls the transaction back")
			assert.False(t, tx.committed)
		})
	}
}

func TestFinishLotCommitFailureKeepsNothing(t *testing.T) {
	tx := dueLotTx()
	tx.commitErr = errFakeDatabase
	pool := &fakePool{factory: func() *fakeTx { return tx }}
	w := testWorker(pool)

	_, found, err := w.finishLot(discardCtx(t))
	require.Error(t, err)
	assert.False(t, found)
	assert.True(t, tx.committed)
	assert.True(t, tx.rolledBack, "the rollback runs after a failed commit")
}

func TestPassStopsAtBatchSize(t *testing.T) {
	pool := &fakePool{factory: dueLotTx}
	w := testWorker(pool)

	finished, err := w.pass(discardCtx(t), 3)
	require.NoError(t, err)
	assert.Equal(t, int64(3), finished)
	assert.Equal(t, 3, pool.beginCount(), "every lot runs in its own transaction")
	assert.Equal(t, int64(3), w.Stats().FinishedLots, "completions are counted after their commit")
	assert.Zero(t, w.Stats().Passes, "the pass counter belongs to Run, not to pass")
}

func TestPassStopsWhenNoWorkLeft(t *testing.T) {
	calls := 0
	pool := &fakePool{factory: func() *fakeTx {
		calls++
		if calls == 1 {
			return dueLotTx()
		}

		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: pgx.ErrNoRows}},
		}}
	}}
	w := testWorker(pool)

	finished, err := w.pass(discardCtx(t), 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), finished)
	assert.Equal(t, 2, pool.beginCount(), "the pass ends with the first empty selection")
}

func TestPassStopsOnError(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: errFakeDatabase}},
		}}
	}}
	w := testWorker(pool)

	_, err := w.pass(discardCtx(t), 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, errFakeDatabase)
	assert.Equal(t, 1, pool.beginCount())
}

func TestRunRunsFirstPassImmediately(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: pgx.ErrNoRows}},
		}}
	}}
	w := testWorker(pool)

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(80*time.Millisecond, cancel)
	defer timer.Stop()

	err := w.Run(ctx, config.Worker{PollInterval: time.Hour, BatchSize: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, pool.beginCount(), "the first pass runs without waiting for the interval")
	snapshot := w.Stats()
	assert.Equal(t, int64(1), snapshot.Passes)
	assert.False(t, snapshot.LastSuccess.IsZero(), "a successful pass without work still updates the success time")
}

func TestRunSurvivesTemporaryDatabaseErrors(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: errFakeDatabase}},
		}}
	}}
	w := testWorker(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := w.Run(ctx, config.Worker{PollInterval: 5 * time.Millisecond, BatchSize: 10})
	require.NoError(t, err, "a temporary database failure stays inside the loop")
	assert.GreaterOrEqual(t, pool.beginCount(), 2, "the loop retries after its bounded pause")
	assert.Equal(t, int64(0), w.Stats().Passes)
	assert.True(t, w.Stats().LastSuccess.IsZero())
}

func TestRunRepeatsSuccessfulPasses(t *testing.T) {
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: pgx.ErrNoRows}},
		}}
	}}
	w := testWorker(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	require.NoError(t, w.Run(ctx, config.Worker{PollInterval: 5 * time.Millisecond, BatchSize: 10}))
	snapshot := w.Stats()
	assert.GreaterOrEqual(t, snapshot.Passes, int64(2), "the loop repeats on the interval")
	assert.False(t, snapshot.LastSuccess.IsZero())
}

func TestSnapshotOfFreshWorker(t *testing.T) {
	w := testWorker(&fakePool{})

	snapshot := w.Stats()
	assert.Equal(t, int64(0), snapshot.FinishedLots)
	assert.Equal(t, int64(0), snapshot.Passes)
	assert.True(t, snapshot.LastSuccess.IsZero())
}
