package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
)

// fakeWorkerMetrics records what the worker reports to the shared instruments.
type fakeWorkerMetrics struct {
	finishes      []finishRecord
	passSuccesses []time.Time
}

type finishRecord struct {
	withWinner bool
	delay      time.Duration
}

func (f *fakeWorkerMetrics) AuctionFinished(withWinner bool, delay time.Duration) {
	f.finishes = append(f.finishes, finishRecord{withWinner: withWinner, delay: delay})
}

func (f *fakeWorkerMetrics) WorkerPassSucceeded(at time.Time) {
	f.passSuccesses = append(f.passSuccesses, at)
}

func newMetricsWorker(pool Pool, metrics *fakeWorkerMetrics) *Worker {
	return New(pool, slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics)
}

func TestMetricsRecordCommittedCompletions(t *testing.T) {
	metrics := &fakeWorkerMetrics{}
	pool := &fakePool{factory: dueLotTx}
	w := newMetricsWorker(pool, metrics)

	finished, err := w.pass(discardCtx(t), 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), finished)

	require.Len(t, metrics.finishes, 1, "one committed completion is recorded")
	assert.True(t, metrics.finishes[0].withWinner, "the lot finished with a winning bid")
	expectedDelay := finishedAt.Sub(dueEndsAt)
	assert.Equal(t, expectedDelay, metrics.finishes[0].delay, "the delay measures finished_at - ends_at")
}

func TestMetricsRecordCompletionsWithoutWinner(t *testing.T) {
	metrics := &fakeWorkerMetrics{}
	pool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{values: []any{int64(7)}}},
			{contains: "SELECT status, ends_at", row: fakeRow{values: []any{"active", dueEndsAt, dbNow}}},
			{contains: "SELECT id, amount FROM bids", row: fakeRow{err: pgx.ErrNoRows}},
			{contains: "UPDATE lots SET", row: fakeRow{values: []any{finishedAt}}},
		}}
	}}
	w := newMetricsWorker(pool, metrics)

	_, err := w.pass(discardCtx(t), 1)
	require.NoError(t, err)

	require.Len(t, metrics.finishes, 1)
	assert.False(t, metrics.finishes[0].withWinner, "a lot without bids finishes without a winner")
}

func TestMetricsPassSuccessAdvancesOnlyOnSuccess(t *testing.T) {
	metrics := &fakeWorkerMetrics{}

	// A successful (empty) pass advances the gauge.
	emptyPool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: pgx.ErrNoRows}},
		}}
	}}
	w := newMetricsWorker(emptyPool, metrics)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Run(ctx, config.Worker{PollInterval: 5 * time.Millisecond, BatchSize: 10}))
	require.NotEmpty(t, metrics.passSuccesses, "a successful pass, even an empty one, advances the success time")

	// A failed pass does not advance it.
	failuresBefore := len(metrics.passSuccesses)
	failingPool := &fakePool{factory: func() *fakeTx {
		return &fakeTx{responses: []fakeTxResponse{
			{contains: "SELECT id FROM lots", row: fakeRow{err: errFakeDatabase}},
		}}
	}}
	w = newMetricsWorker(failingPool, metrics)
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Run(ctx, config.Worker{PollInterval: 5 * time.Millisecond, BatchSize: 10}))
	assert.Len(t, metrics.passSuccesses, failuresBefore, "a failed pass never advances the success time")
	assert.Empty(t, metrics.finishes, "a pass without commits records no completions")
}

func TestMetricsNilImplementationIsTolerated(t *testing.T) {
	pool := &fakePool{factory: dueLotTx}
	w := New(pool, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)

	finished, err := w.pass(discardCtx(t), 3)
	require.NoError(t, err)
	assert.Equal(t, int64(3), finished, "a nil metrics dependency changes nothing about the work")
}
