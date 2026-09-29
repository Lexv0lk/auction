//go:build integration

package lot

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/testutil"
)

// createBidUser inserts a participant account directly and returns its ID.
func createBidUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, login string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ($1, 'integration-hash', 'participant') RETURNING id",
		login).Scan(&id)
	require.NoError(t, err)

	return id
}

// createLotWithStatus inserts a lot in the requested state directly, with any
// deadline; finished lots get a finished_at to satisfy the result constraint.
func createLotWithStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, categoryID int64, status string, startPrice int64, endsAt time.Time) int64 {
	t.Helper()

	finishedAt := "NULL"
	if status == StatusFinished {
		finishedAt = "now()"
	}

	var id int64
	err := pool.QueryRow(ctx,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at, finished_at)"+
			" VALUES ('Тестовый лот', 'Описание тестового лота', $1, $2, $3, $4, "+finishedAt+") RETURNING id",
		categoryID, startPrice, status, endsAt).Scan(&id)
	require.NoError(t, err)

	return id
}

func countBids(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) int64 {
	t.Helper()

	var count int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM bids WHERE lot_id = $1", lotID).Scan(&count))

	return count
}

func maxAcceptedBid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID int64) int64 {
	t.Helper()

	var maxAmount int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(amount), 0) FROM bids WHERE lot_id = $1", lotID).Scan(&maxAmount))

	return maxAmount
}

func TestPlaceBidPricesAgainstStartAndMax(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")
	other := createBidUser(t, ctx, pool, "bidder-2")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))
	key := "6f1c0e64-1f2d-4c1a-9e5e-8d2b3c4a5f6b"

	// Zero and negative amounts are refused before anything is written.
	for _, amount := range []int64{0, -1} {
		_, err := service.PlaceBid(ctx, user, lotID, amount, key)
		assert.ErrorIs(t, err, ErrBidAmountInvalid)
	}

	// A rejected bid writes nothing and does not reserve the request key.
	_, err := service.PlaceBid(ctx, user, lotID, 99, key)
	assert.ErrorIs(t, err, ErrBidTooLow)
	assert.Zero(t, countBids(t, ctx, pool, lotID))

	// The first accepted bid may equal the start price; accepted_at comes
	// from the database clock between the two samples.
	var dbBefore, dbAfter time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&dbBefore))
	first, err := service.PlaceBid(ctx, user, lotID, 100, key)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&dbAfter))
	assert.Positive(t, first.ID)
	assert.False(t, first.Repeated)
	assert.Equal(t, lotID, first.LotID)
	assert.Equal(t, user, first.ParticipantID)
	assert.Equal(t, int64(100), first.Amount)
	assert.Equal(t, key, first.RequestKey)
	assert.GreaterOrEqual(t, first.Duration, time.Duration(0))
	assert.False(t, first.AcceptedAt.Before(dbBefore), "accepted_at comes from the database clock")
	assert.False(t, first.AcceptedAt.After(dbAfter), "accepted_at comes from the database clock")

	// An equal amount from another participant is too low now; one more is
	// accepted, and the previously rejected key stayed free for it.
	_, err = service.PlaceBid(ctx, other, lotID, 100, key)
	assert.ErrorIs(t, err, ErrBidTooLow)
	second, err := service.PlaceBid(ctx, other, lotID, 101, key)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, int64(2), countBids(t, ctx, pool, lotID))
	assert.Equal(t, int64(101), maxAcceptedBid(t, ctx, pool, lotID))
}

func TestPlaceBidRepeatAndConflict(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")
	other := createBidUser(t, ctx, pool, "bidder-2")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))
	key := "2b1f0c9a-3d4e-4f5a-8b6c-7d8e9f0a1b2c"

	first, err := service.PlaceBid(ctx, user, lotID, 100, key)
	require.NoError(t, err)

	// The same key and amount return the stored bid, not a new record.
	repeat, err := service.PlaceBid(ctx, user, lotID, 100, key)
	require.NoError(t, err)
	assert.Equal(t, first.ID, repeat.ID)
	assert.True(t, repeat.Repeated)
	assert.True(t, first.AcceptedAt.Equal(repeat.AcceptedAt))
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))

	// The same key with another amount is a conflict and writes nothing.
	_, err = service.PlaceBid(ctx, user, lotID, 150, key)
	assert.ErrorIs(t, err, ErrRequestKeyConflict)
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))

	// The key is scoped by participant and lot: another participant may use
	// the same key.
	_, err = service.PlaceBid(ctx, other, lotID, 120, key)
	require.NoError(t, err)
	assert.Equal(t, int64(2), countBids(t, ctx, pool, lotID))

	// A repeat of the original bid still returns it after the lot is
	// finished: the repeat is the answer to the old attempt, not a new bid.
	_, err = pool.Exec(ctx, "UPDATE lots SET status = 'finished', finished_at = now() WHERE id = $1", lotID)
	require.NoError(t, err)
	repeatAfter, err := service.PlaceBid(ctx, user, lotID, 100, key)
	require.NoError(t, err)
	assert.Equal(t, first.ID, repeatAfter.ID)
	assert.True(t, repeatAfter.Repeated)
	assert.Equal(t, int64(2), countBids(t, ctx, pool, lotID), "the repeat does not add a record")
}

func TestPlaceBidRejectsMissingDraftAndClosedLots(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")

	// A draft is answered like a missing lot: the direct access reveals
	// nothing about unpublished lots.
	draftID := createLotWithStatus(t, ctx, pool, categoryID, StatusDraft, 100, time.Now().Add(time.Hour))
	_, err := service.PlaceBid(ctx, user, draftID, 100, "a1000000-0000-4000-8000-000000000001")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = service.PlaceBid(ctx, user, 999999, 100, "a1000000-0000-4000-8000-000000000002")
	assert.ErrorIs(t, err, ErrNotFound)

	// A finished lot refuses new bids.
	finishedID := createLotWithStatus(t, ctx, pool, categoryID, StatusFinished, 100, time.Now().Add(-time.Hour))
	_, err = service.PlaceBid(ctx, user, finishedID, 100, "a1000000-0000-4000-8000-000000000003")
	assert.ErrorIs(t, err, ErrBiddingClosed)

	// An active lot whose deadline has passed (the result is being
	// determined) refuses new bids as well.
	dueID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(-time.Minute))
	_, err = service.PlaceBid(ctx, user, dueID, 100, "a1000000-0000-4000-8000-000000000004")
	assert.ErrorIs(t, err, ErrBiddingClosed)

	// A bid referencing a missing participant is refused by the foreign key.
	activeID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))
	_, err = service.PlaceBid(ctx, 999999, activeID, 100, "a1000000-0000-4000-8000-000000000005")
	assert.ErrorIs(t, err, ErrParticipantMissing)

	assert.Zero(t, countBids(t, ctx, pool, draftID))
	assert.Zero(t, countBids(t, ctx, pool, finishedID))
	assert.Zero(t, countBids(t, ctx, pool, dueID))
	assert.Zero(t, countBids(t, ctx, pool, activeID))
}

func TestPlaceBidSameAmountRace(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	first := createBidUser(t, ctx, pool, "bidder-1")
	second := createBidUser(t, ctx, pool, "bidder-2")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))

	start := make(chan struct{})
	var wg sync.WaitGroup
	var firstBid, secondBid PlacedBid
	var firstErr, secondErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		firstBid, firstErr = service.PlaceBid(ctx, first, lotID, 150, "c1000000-0000-4000-8000-000000000001")
	}()
	go func() {
		defer wg.Done()
		<-start
		secondBid, secondErr = service.PlaceBid(ctx, second, lotID, 150, "c1000000-0000-4000-8000-000000000002")
	}()
	close(start)
	wg.Wait()

	// Exactly one participant wins the amount: the other is refused by the
	// price check under the row lock.
	accepted, rejected := 0, 0
	for _, placedErr := range []error{firstErr, secondErr} {
		switch {
		case placedErr == nil:
			accepted++
		case errors.Is(placedErr, ErrBidTooLow):
			rejected++
		default:
			require.NoError(t, placedErr, "the race resolves only into acceptance or a price refusal")
		}
	}
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 1, rejected)
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))
	assert.Equal(t, int64(150), maxAcceptedBid(t, ctx, pool, lotID))
	_ = firstBid
	_ = secondBid
}

func TestPlaceBidRisingAmountsRace(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))

	const participants = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, participants)
	wg.Add(participants)
	for i := 0; i < participants; i++ {
		userID := createBidUser(t, ctx, pool, fmt.Sprintf("bidder-%d", i+1))
		amount := int64(101 + i)
		key := fmt.Sprintf("d0000000-0000-4000-8000-%012d", i+1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = service.PlaceBid(ctx, userID, lotID, amount, key)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrBidTooLow) {
			require.NoError(t, err, "participant %d failed technically", i+1)
		}
	}

	// Every accepted bid raised the price: the stored history is strictly
	// increasing in insertion order, and the highest submitted amount is
	// always accepted because nothing can outrun it. Not every request must
	// be accepted — that depends on the arrival order.
	type bidRow struct {
		id     int64
		amount int64
	}
	rows := []bidRow{}
	gotRows, err := pool.Query(ctx, "SELECT id, amount FROM bids WHERE lot_id = $1 ORDER BY id", lotID)
	require.NoError(t, err)
	defer gotRows.Close()
	for gotRows.Next() {
		var row bidRow
		require.NoError(t, gotRows.Scan(&row.id, &row.amount))
		rows = append(rows, row)
	}
	require.NoError(t, gotRows.Err())

	require.NotEmpty(t, rows, "the highest amount is always accepted")
	for i := 1; i < len(rows); i++ {
		assert.Greater(t, rows[i].amount, rows[i-1].amount, "an accepted bid always raised the price")
	}
	assert.Equal(t, int64(104), rows[len(rows)-1].amount)
	assert.Equal(t, int64(104), maxAcceptedBid(t, ctx, pool, lotID))
}

func TestPlaceBidConcurrentRepeatsReturnOneBid(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))
	const key = "e1000000-0000-4000-8000-000000000001"

	const attempts = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	bids := make([]PlacedBid, attempts)
	errs := make([]error, attempts)
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			<-start
			bids[i], errs[i] = service.PlaceBid(ctx, user, lotID, 130, key)
		}()
	}
	close(start)
	wg.Wait()

	// Every attempt returns the same stored bid; exactly one attempt created
	// it, the row lock serialized the rest into repeats.
	newBids := 0
	for i, err := range errs {
		require.NoError(t, err, "repeat %d must return the stored bid", i+1)
		assert.Equal(t, bids[0].ID, bids[i].ID, "all repeats return one ID")
		if !bids[i].Repeated {
			newBids++
		}
	}
	assert.Equal(t, 1, newBids)
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))
}

func TestPlaceBidStartedBeforeDeadlineIsRefusedAfterIt(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")
	endsAt := time.Now().Add(2 * time.Second)
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, endsAt)

	// The holder keeps the lot row locked past the deadline: the bid started
	// before the deadline and can only reach the time check after the lock
	// is released.
	holder, err := pool.Begin(ctx)
	require.NoError(t, err)
	var lockedID int64
	require.NoError(t, holder.QueryRow(ctx, "SELECT id FROM lots WHERE id = $1 FOR UPDATE", lotID).Scan(&lockedID))

	type placedResult struct {
		bid PlacedBid
		err error
	}
	done := make(chan placedResult, 1)
	go func() {
		bidCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		bid, err := service.PlaceBid(bidCtx, user, lotID, 150, "f1000000-0000-4000-8000-000000000001")
		done <- placedResult{bid: bid, err: err}
	}()

	// Wait until the database clock passes the deadline, then let the bid in.
	for {
		var dbNow time.Time
		require.NoError(t, pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&dbNow))
		if !dbNow.Before(endsAt) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, holder.Commit(ctx))

	select {
	case result := <-done:
		assert.ErrorIs(t, result.err, ErrBiddingClosed, "the lock was released only after the deadline")
		assert.False(t, result.bid.Repeated)
	case <-time.After(20 * time.Second):
		t.Fatal("the bid never finished")
	}
	assert.Zero(t, countBids(t, ctx, pool, lotID))

	// A fresh bid after the deadline is refused by the same time condition.
	_, err = service.PlaceBid(ctx, user, lotID, 150, "f1000000-0000-4000-8000-000000000002")
	assert.ErrorIs(t, err, ErrBiddingClosed)
	assert.Zero(t, countBids(t, ctx, pool, lotID))
}

func TestPlaceBidRejectedAttemptReleasesLockAndKey(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	user := createBidUser(t, ctx, pool, "bidder-1")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 100, time.Now().Add(time.Hour))
	key := "a2000000-0000-4000-8000-000000000001"

	// A rejected bid writes nothing.
	_, err := service.PlaceBid(ctx, user, lotID, 50, key)
	require.ErrorIs(t, err, ErrBidTooLow)
	assert.Zero(t, countBids(t, ctx, pool, lotID))

	// The rollback released the row lock: a probe transaction locks the lot
	// within the bounded context instead of waiting forever.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 3*time.Second)
	probe, err := pool.Begin(probeCtx)
	require.NoError(t, err, "the rejected bid released the row lock")
	var lockedID int64
	require.NoError(t, probe.QueryRow(probeCtx, "SELECT id FROM lots WHERE id = $1 FOR UPDATE", lotID).Scan(&lockedID))
	require.NoError(t, probe.Rollback(probeCtx))
	cancelProbe()

	// The rejected attempt did not reserve the request key: the same key
	// accepts the valid amount.
	placed, err := service.PlaceBid(ctx, user, lotID, 100, key)
	require.NoError(t, err)
	assert.False(t, placed.Repeated)

	// Fixing the deadline at a captured instant of the database clock closes
	// the bidding for every later statement: the comparison is strict (<), so
	// a bid exactly on the boundary instant is not accepted either.
	var captured time.Time
	require.NoError(t, pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&captured))
	_, err = pool.Exec(ctx, "UPDATE lots SET ends_at = $1 WHERE id = $2", captured, lotID)
	require.NoError(t, err)
	_, err = service.PlaceBid(ctx, user, lotID, 101, "a2000000-0000-4000-8000-000000000002")
	assert.ErrorIs(t, err, ErrBiddingClosed, "a deadline fixed at a past instant refuses every later bid")
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))
}

func TestPlaceBidDoesNotOverflowAtMaxPrice(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")
	first := createBidUser(t, ctx, pool, "bidder-1")
	second := createBidUser(t, ctx, pool, "bidder-2")
	lotID := createLotWithStatus(t, ctx, pool, categoryID, StatusActive, 1, time.Now().Add(time.Hour))

	placed, err := service.PlaceBid(ctx, first, lotID, math.MaxInt64, "b3000000-0000-4000-8000-000000000001")
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), placed.Amount)

	// The next bid cannot exceed the stored maximum: the comparison never
	// builds max+1, so no overflow happens and the refusal is a plain price
	// refusal.
	_, err = service.PlaceBid(ctx, second, lotID, math.MaxInt64, "b3000000-0000-4000-8000-000000000002")
	assert.ErrorIs(t, err, ErrBidTooLow)
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))
}
