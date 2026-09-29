// Package worker completes the auctions whose deadline has passed. The
// worker runs as one goroutine inside every server process next to HTTP: the
// same pool, the same shutdown budget, no separate binary or profile. Every
// lot is finished in its own short transaction, so a restart loses nothing —
// the database is the only work list, and the next pass (or the next
// replica) picks up whatever is still due.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/lot"
)

// lotTxTimeout bounds one lot-finishing transaction; a pass of BatchSize lots
// never holds one transaction or one connection longer than this.
const lotTxTimeout = 5 * time.Second

// rollbackTimeout bounds the independent rollback context: when the
// application is shutting down the parent context is already cancelled, and
// the row lock must still be released for the next replica.
const rollbackTimeout = 2 * time.Second

// The statements follow the fixed lock order of the bid operation: the lot
// row is locked first, the connected bid data is read under the lock, and
// only then the lot row is updated. The selection walks the partial index
// lots_active_ends_at_idx (ends_at, id) WHERE status = 'active': finished
// lots are never selected again, and SKIP LOCKED turns a lot held by another
// replica into "no work" instead of a wait.
const (
	selectDueLotSQL = "SELECT id FROM lots" +
		" WHERE status = 'active' AND ends_at <= now()" +
		" ORDER BY ends_at, id LIMIT 1 FOR UPDATE SKIP LOCKED"

	// The lock holder re-verifies eligibility by the database clock: the
	// decision belongs to now() of the transaction, never to the caller.
	verifyLockedLotSQL = "SELECT status, ends_at, clock_timestamp() FROM lots WHERE id = $1 FOR UPDATE"

	// The maximal accepted bid wins; ties on amount go to the latest
	// accepted one. The index bids_lot_amount_idx serves this read directly.
	selectWinningBidSQL = "SELECT id, amount FROM bids WHERE lot_id = $1" +
		" ORDER BY amount DESC, id DESC LIMIT 1"

	// finished_at comes from the PostgreSQL clock; a lot without bids keeps
	// the NULL winner (the composite FK ignores a NULL member).
	finishLotSQL = "UPDATE lots SET status = 'finished', finished_at = clock_timestamp(), winning_bid_id = $2" +
		" WHERE id = $1 RETURNING finished_at"
)

// Pool is the subset of the connection pool the worker needs: every lot
// finishes inside its own transaction with the pinned isolation level.
type Pool interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// Metrics is the observability contract of the worker: the application
// supplies the shared Prometheus instruments, a nil value disables recording.
// The completion counter belongs after the commit and the success gauge is
// advanced by successful passes only (an empty pass included).
type Metrics interface {
	AuctionFinished(withWinner bool, delay time.Duration)
	WorkerPassSucceeded(at time.Time)
}

// Worker finishes due auctions over the shared pool. The atomics carry the
// observability signals; the database carries everything else.
type Worker struct {
	pool    Pool
	logger  *slog.Logger
	metrics Metrics

	finishedLots atomic.Int64 // completions counted after their commit
	passes       atomic.Int64 // successful passes, including passes without work
	lastSuccess  atomic.Int64 // UnixNano of the last successful pass; 0 = none yet
}

// Snapshot is a point-in-time reading of the worker's observability signals:
// the number of committed completions, the number of successful passes and
// the time of the last successful pass (a pass without work is a success).
type Snapshot struct {
	FinishedLots int64
	Passes       int64
	LastSuccess  time.Time
}

// New builds the worker on top of the shared connection pool.
func New(pool Pool, logger *slog.Logger, metrics Metrics) *Worker {
	return &Worker{pool: pool, logger: logger, metrics: metrics}
}

// Stats reports the observability signals of the worker so far.
func (w *Worker) Stats() Snapshot {
	snapshot := Snapshot{FinishedLots: w.finishedLots.Load(), Passes: w.passes.Load()}
	if nanos := w.lastSuccess.Load(); nanos != 0 {
		snapshot.LastSuccess = time.Unix(0, nanos)
	}

	return snapshot
}

// finishedLot is one committed completion: the winner fields echo what the
// transaction stored, and Delay measures how long after the deadline the
// result was recorded.
type finishedLot struct {
	LotID         int64
	WinningBidID  *int64
	WinningAmount int64
	FinishedAt    time.Time
	EndsAt        time.Time
}

// Delay is the time between the deadline and the recorded finish; it is
// always positive for a lot finished by this worker.
func (f finishedLot) Delay() time.Duration {
	return f.FinishedAt.Sub(f.EndsAt)
}

// Run loops the completion passes until the context is cancelled: the first
// pass starts immediately (a restarted instance catches up without waiting),
// every later pass waits PollInterval. A temporary database failure aborts
// only the current pass — it is logged, the transaction is rolled back and
// the loop retries after the same bounded pause. Run returns nil when the
// application context is done; a background loop has no failure that would
// justify an exit on its own.
func (w *Worker) Run(ctx context.Context, cfg config.Worker) error {
	for {
		started := time.Now()
		completions, err := w.pass(ctx, cfg.BatchSize)
		if ctx.Err() != nil {
			//nolint:nilerr // the pass error is the context cancellation itself: the application is stopping, which is a normal shutdown, never a failure
			return nil
		}
		if err == nil {
			w.passes.Add(1)
			now := time.Now()
			w.lastSuccess.Store(now.UnixNano())
			if w.metrics != nil {
				w.metrics.WorkerPassSucceeded(now)
			}
			w.logger.Info("worker pass completed",
				"operation", "worker_pass", "outcome", "ok",
				"finished_lots", completions,
				"duration", time.Since(started).Milliseconds())
		} else {
			w.logger.Error("worker pass failed",
				"operation", "worker_pass", "outcome", "error",
				"error", err.Error(),
				"finished_lots", completions)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.PollInterval):
		}
	}
}

// pass finishes up to batchSize due lots and reports how many it finished.
// Every lot runs in its own short transaction: no pass ever holds locks of
// several lots, and no transaction is left open between iterations. An empty
// selection ends the pass — the skipped locked lots of other replicas are
// picked up by one of the next passes.
func (w *Worker) pass(ctx context.Context, batchSize int) (int64, error) {
	var completions int64
	for completions < int64(batchSize) {
		if ctx.Err() != nil {
			return completions, ctx.Err()
		}
		outcome, found, err := w.finishLot(ctx)
		if err != nil {
			return completions, err
		}
		if !found {
			return completions, nil
		}
		completions++
		w.recordCompletion(outcome)
	}

	return completions, nil
}

// finishLot completes one due lot inside one READ COMMITTED transaction. The
// selection locks the oldest due lot with FOR UPDATE SKIP LOCKED; a missing
// row is a normal "no work" answer, not an error. Under the lock the
// eligibility is re-verified against the database clock, the maximal
// accepted bid is read (its absence keeps the NULL winner) and the lot is
// stored as finished with the PostgreSQL clock instant — all fields of the
// result are fixed atomically by the single commit.
func (w *Worker) finishLot(ctx context.Context) (finishedLot, bool, error) {
	txCtx, cancelTx := context.WithTimeout(ctx, lotTxTimeout)
	defer cancelTx()

	tx, err := w.pool.BeginTx(txCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return finishedLot{}, false, fmt.Errorf("begin finish transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancelRollback()
		_ = tx.Rollback(rollbackCtx)
	}()

	var lotID int64
	err = tx.QueryRow(txCtx, selectDueLotSQL).Scan(&lotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return finishedLot{}, false, nil
	}
	if err != nil {
		return finishedLot{}, false, fmt.Errorf("select due lot: %w", err)
	}

	var status string
	var endsAt, dbNow time.Time
	if err := tx.QueryRow(txCtx, verifyLockedLotSQL, lotID).Scan(&status, &endsAt, &dbNow); err != nil {
		return finishedLot{}, false, fmt.Errorf("re-verify locked lot: %w", err)
	}
	if status != lot.StatusActive || endsAt.After(dbNow) {
		// The candidate changed between the selection snapshot and the lock,
		// or the database clock disagrees: leave it for the next pass.
		return finishedLot{}, false, nil
	}

	var winningBidID *int64
	var winningAmount int64
	err = tx.QueryRow(txCtx, selectWinningBidSQL, lotID).Scan(&winningBidID, &winningAmount)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return finishedLot{}, false, fmt.Errorf("select winning bid: %w", err)
	}

	var finishedAt time.Time
	if err := tx.QueryRow(txCtx, finishLotSQL, lotID, winningBidID).Scan(&finishedAt); err != nil {
		return finishedLot{}, false, fmt.Errorf("finish lot: %w", err)
	}

	if err := tx.Commit(txCtx); err != nil {
		return finishedLot{}, false, fmt.Errorf("commit finish transaction: %w", err)
	}

	return finishedLot{
		LotID:         lotID,
		WinningBidID:  winningBidID,
		WinningAmount: winningAmount,
		FinishedAt:    finishedAt,
		EndsAt:        endsAt,
	}, true, nil
}

// recordCompletion logs one committed completion and moves the counters: the
// completion is counted only after its commit confirmed the result.
func (w *Worker) recordCompletion(outcome finishedLot) {
	w.finishedLots.Add(1)
	if w.metrics != nil {
		w.metrics.AuctionFinished(outcome.WinningBidID != nil, outcome.Delay())
	}
	var winningBid any
	if outcome.WinningBidID != nil {
		winningBid = *outcome.WinningBidID
	}
	w.logger.Info("lot finished",
		"operation", "finish_lot", "outcome", "finished",
		"lot_id", outcome.LotID,
		"winning_bid_id", winningBid,
		"winning_amount", outcome.WinningAmount,
		"completion_delay", outcome.Delay().Milliseconds())
}
