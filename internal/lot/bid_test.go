package lot

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validBidKey() string {
	return "0f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1f"
}

func bidLockRow(startPrice int64, status string, endsAt time.Time) fakeRow {
	return fakeRow{values: []any{startPrice, status, endsAt}}
}

// bidTx builds a transaction fake for one PlaceBid run: the lot lock answers
// the startPrice/status/endsAt triple, and each statement of the bid
// sequence gets its own response by a SQL substring.
func bidTx(t *testing.T, lockRow fakeRow, repeatRow, maxRow, insertRow fakeRow) *fakeTx {
	t.Helper()

	return &fakeTx{
		lockRow: lockRow,
		responses: []fakeTxResponse{
			{contains: "FROM bids WHERE user_id", row: repeatRow},
			{contains: "MAX(amount)", row: maxRow},
			{contains: "INSERT INTO bids", row: insertRow},
		},
	}
}

func TestPlaceBidValidatesInputBeforeSQL(t *testing.T) {
	pool := &fakePool{}
	service := NewService(pool)

	for _, participant := range []int64{0, -5} {
		_, err := service.PlaceBid(context.Background(), participant, 3, 100, validBidKey())
		assert.ErrorIs(t, err, ErrParticipantRequired)
	}
	for _, amount := range []int64{0, -1} {
		_, err := service.PlaceBid(context.Background(), 5, 3, amount, validBidKey())
		assert.ErrorIs(t, err, ErrBidAmountInvalid)
	}
	for _, key := range []string{
		"",
		"not-a-uuid",
		"0f0e0d0c0b0a493882716a5b4c3d2e1f",
		"0f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1",
		"0f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1fx",
		"-f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1f",
		"gf0e0d0c-0b0a-4938-8271-6a5b4c3d2e1f",
	} {
		_, err := service.PlaceBid(context.Background(), 5, 3, 100, key)
		assert.ErrorIs(t, err, ErrRequestKeyInvalid)
	}

	assert.Empty(t, pool.queries, "validation runs before any SQL")
	assert.Nil(t, pool.tx, "no transaction starts for invalid input")
}

func TestPlaceBidLocksChecksAndInsertsInOrder(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	acceptedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tx := bidTx(t,
		bidLockRow(100, StatusActive, endsAt),
		fakeRow{err: pgx.ErrNoRows},
		fakeRow{values: []any{int64(0)}},
		fakeRow{values: []any{int64(9), acceptedAt}},
	)
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	placed, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	require.NoError(t, err)

	assert.Equal(t, int64(9), placed.ID)
	assert.Equal(t, int64(3), placed.LotID)
	assert.Equal(t, int64(5), placed.ParticipantID)
	assert.Equal(t, int64(100), placed.Amount)
	assert.Equal(t, validBidKey(), placed.RequestKey)
	assert.True(t, acceptedAt.Equal(placed.AcceptedAt))
	assert.False(t, placed.Repeated, "a new bid is not a repeat")
	assert.GreaterOrEqual(t, placed.Duration, time.Duration(0))
	assert.True(t, tx.committed, "success is returned only after the confirmed commit")

	// The fixed order: the lot row first, then the repeat lookup, the
	// maximal accepted amount, and only then the insert.
	require.Len(t, tx.queries, 4)
	assert.Contains(t, tx.queries[0].sql, "FOR UPDATE")
	assert.Contains(t, tx.queries[1].sql, "FROM bids WHERE user_id")
	assert.Contains(t, tx.queries[2].sql, "MAX(amount)")
	assert.Contains(t, tx.queries[3].sql, "INSERT INTO bids")

	insertArgs := tx.queries[3].args
	require.Len(t, insertArgs, 5)
	assert.Equal(t, int64(3), insertArgs[0])
	assert.Equal(t, int64(5), insertArgs[1])
	assert.Equal(t, int64(100), insertArgs[2])
	assert.Equal(t, validBidKey(), insertArgs[3])
	assert.True(t, endsAt.Equal(insertArgs[4].(time.Time)))

	assert.Equal(t, pgx.ReadCommitted, pool.txOptions.IsoLevel,
		"the bid transaction pins READ COMMITTED: the statements after the granted lock must see the previous holder's commit")
}

func TestPlaceBidReturnsStoredBidForSameKey(t *testing.T) {
	acceptedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tx := bidTx(t,
		bidLockRow(100, StatusActive, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{values: []any{int64(9), int64(100), acceptedAt}},
		fakeRow{},
		fakeRow{},
	)
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	placed, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	require.NoError(t, err)

	assert.Equal(t, int64(9), placed.ID)
	assert.True(t, placed.Repeated)
	assert.True(t, acceptedAt.Equal(placed.AcceptedAt))
	assert.True(t, tx.committed)

	// The repeat finishes the transaction right after the lookup: no state
	// check, no price query, no insert — it returns the former result as it
	// was stored.
	require.Len(t, tx.queries, 2)
	assert.Contains(t, tx.queries[0].sql, "FOR UPDATE")
	assert.Contains(t, tx.queries[1].sql, "FROM bids WHERE user_id")
}

func TestPlaceBidConflictsOnSameKeyDifferentAmount(t *testing.T) {
	acceptedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tx := bidTx(t,
		bidLockRow(100, StatusActive, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{values: []any{int64(9), int64(100), acceptedAt}},
		fakeRow{},
		fakeRow{},
	)
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.PlaceBid(context.Background(), 5, 3, 150, validBidKey())
	assert.ErrorIs(t, err, ErrRequestKeyConflict)
	assert.False(t, tx.committed)
	assert.True(t, tx.rolledBack)
	require.Len(t, tx.queries, 2, "the conflict is answered without touching the price or inserting")
}

func TestPlaceBidRefusesInactiveLotBeforePrice(t *testing.T) {
	tx := bidTx(t,
		bidLockRow(100, StatusFinished, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{err: pgx.ErrNoRows},
		fakeRow{},
		fakeRow{},
	)
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	assert.ErrorIs(t, err, ErrBiddingClosed)
	assert.False(t, tx.committed)
	assert.True(t, tx.rolledBack)
	require.Len(t, tx.queries, 2, "the refusal happens after the repeat lookup and before any price query")
}

func TestPlaceBidAnswersMissingForUnknownLot(t *testing.T) {
	tx := &fakeTx{lockRow: fakeRow{err: pgx.ErrNoRows}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	assert.ErrorIs(t, err, ErrNotFound)
	require.Len(t, tx.queries, 1)
	assert.False(t, tx.committed)
	assert.True(t, tx.rolledBack)
}

func TestPlaceBidPricesAgainstStartAndMaxAmounts(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	acceptedAt := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		startPrice int64
		maxAmount  int64
		amount     int64
		wantErr    error
	}{
		{name: "below the start price", startPrice: 100, maxAmount: 0, amount: 99, wantErr: ErrBidTooLow},
		{name: "equal to the start price", startPrice: 100, maxAmount: 0, amount: 100},
		{name: "equal to the current price", startPrice: 100, maxAmount: 150, amount: 150, wantErr: ErrBidTooLow},
		{name: "one above the current price", startPrice: 100, maxAmount: 150, amount: 151},
		{name: "equal to the maximal int64 price", startPrice: 1, maxAmount: math.MaxInt64, amount: math.MaxInt64, wantErr: ErrBidTooLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := bidTx(t,
				bidLockRow(tt.startPrice, StatusActive, endsAt),
				fakeRow{err: pgx.ErrNoRows},
				fakeRow{values: []any{tt.maxAmount}},
				fakeRow{values: []any{int64(9), acceptedAt}},
			)
			pool := &fakePool{tx: tx}
			service := NewService(pool)

			_, err := service.PlaceBid(context.Background(), 5, 3, tt.amount, validBidKey())
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.False(t, tx.committed)

				return
			}
			require.NoError(t, err)
			assert.True(t, tx.committed)
			require.Len(t, tx.queries, 4)
		})
	}
}

func TestPlaceBidRefusesWhenDeadlineConditionRejectsInsert(t *testing.T) {
	tx := bidTx(t,
		bidLockRow(100, StatusActive, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{err: pgx.ErrNoRows},
		fakeRow{values: []any{int64(0)}},
		fakeRow{err: pgx.ErrNoRows},
	)
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	assert.ErrorIs(t, err, ErrBiddingClosed,
		"zero inserted rows mean the database clock passed the deadline")
	require.Len(t, tx.queries, 4)
	assert.False(t, tx.committed)
	assert.True(t, tx.rolledBack)
}

func TestBidInsertComputesTimeOnceWithStrictComparison(t *testing.T) {
	assert.Contains(t, insertBidSQL, "clock_timestamp()",
		"the final time is the running PostgreSQL clock, not now() and not the Go clock")
	assert.Contains(t, insertBidSQL, "ts < ",
		"the boundary instant itself refuses the bid: only ts < ends_at passes")
	assert.Contains(t, insertBidSQL, "RETURNING id, accepted_at",
		"the stored accepted_at is the same instant the condition used")
}

func TestPlaceBidWrapsTechnicalFailures(t *testing.T) {
	service := NewService(&fakePool{beginErr: errBeginFailed})
	_, err := service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	assert.ErrorIs(t, err, errBeginFailed)

	tx := bidTx(t,
		bidLockRow(100, StatusActive, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{err: errDatabaseDown},
		fakeRow{},
		fakeRow{},
	)
	service = NewService(&fakePool{tx: tx})
	_, err = service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	assert.ErrorIs(t, err, errDatabaseDown)

	tx = bidTx(t,
		bidLockRow(100, StatusActive, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)),
		fakeRow{err: pgx.ErrNoRows},
		fakeRow{values: []any{int64(0)}},
		fakeRow{values: []any{int64(9), time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}},
	)
	tx.commitErr = errCommitFailed
	service = NewService(&fakePool{tx: tx})
	_, err = service.PlaceBid(context.Background(), 5, 3, 100, validBidKey())
	// The commit outcome is unknown: the caller must retry with the same
	// key, so the error is never nil and never a typed refusal.
	assert.ErrorIs(t, err, errCommitFailed)
	assert.NotErrorIs(t, err, ErrBidTooLow)
}

func TestValidRequestKey(t *testing.T) {
	assert.True(t, validRequestKey("0f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1f"))
	assert.True(t, validRequestKey("0F0E0D0C-0B0A-4938-8271-6A5B4C3D2E1F"), "upper-case hex is the same UUID format")
	assert.True(t, validRequestKey("00000000-0000-0000-0000-000000000000"))
	assert.False(t, validRequestKey("0f0e0d0c-0b0a-4938-8271-6a5b4c3d2e1g"), "g is not a hex digit")
	assert.False(t, validRequestKey("0f0e0d0c:0b0a-4938-8271-6a5b4c3d2e1f"), "the separators are hyphens")
}
