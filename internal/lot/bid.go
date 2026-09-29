package lot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outcomes of the bid operation beyond the stored bid. Money is an int64 by
// contract and never travels as float64; a refusal never reserves the request
// key and never writes anything, because every refusal leaves the transaction
// rolled back.
var (
	// ErrParticipantRequired means the call carries no participant identity;
	// the identity always comes from the authenticated session, never from
	// the request body.
	ErrParticipantRequired = errors.New("bid participant is required")
	// ErrParticipantMissing means the referenced participant does not exist;
	// the foreign key on bids.user_id is the final guard.
	ErrParticipantMissing = errors.New("bid participant does not exist")
	// ErrBidAmountInvalid means the amount is zero or negative.
	ErrBidAmountInvalid = errors.New("bid amount must be a positive integer")
	// ErrRequestKeyInvalid means the request key is not a canonical UUID.
	ErrRequestKeyInvalid = errors.New("bid request key is not a UUID")
	// ErrRequestKeyConflict means the key is already stored for this
	// participant and lot with a different amount: an issued key never
	// changes its amount.
	ErrRequestKeyConflict = errors.New("bid request key is already used with a different amount")
	// ErrBidTooLow means the amount does not reach the required minimum: the
	// start price for the first bid, a value above the maximal accepted
	// amount afterwards.
	ErrBidTooLow = errors.New("bid amount is below the required minimum")
	// ErrBiddingClosed means the lot no longer accepts bids: the deadline has
	// passed or the worker has finished the lot.
	ErrBiddingClosed = errors.New("the bidding is closed for the lot")
)

// The bid follows the one fixed lock order of the lot operations: the lot row
// is locked first, the connected data is read under the lock and only then
// written. The final INSERT computes the running PostgreSQL clock once
// (clock_timestamp), compares it strictly with the deadline and stores the
// same instant in accepted_at; zero inserted rows mean the bidding is closed.
const (
	// A draft is filtered out here: a direct bid answers it like a missing
	// lot and never reveals its existence.
	lockLotForBidSQL = "SELECT start_price, status, ends_at" +
		" FROM lots WHERE id = $1 AND status <> 'draft' FOR UPDATE"

	// The request key is unique per participant and lot, so this lookup is
	// the idempotency record of the operation.
	selectBidByKeySQL = "SELECT id, amount, accepted_at FROM bids" +
		" WHERE user_id = $1 AND lot_id = $2 AND request_key = $3"

	selectMaxBidSQL = "SELECT COALESCE(MAX(amount), 0) FROM bids WHERE lot_id = $1"

	// The subquery computes the time once: the WHERE condition and the
	// accepted_at column use exactly the same instant.
	insertBidSQL = "INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key)" +
		" SELECT $1, $2, $3, ts, $4 FROM (SELECT clock_timestamp() AS ts) once" +
		" WHERE ts < $5 RETURNING id, accepted_at"
)

// PlacedBid is the bid as the operation stored (or found) it. Repeated marks
// the return of an earlier accepted bid for the same participant, lot and
// request key. Duration is the time the operation spent from entry to the
// confirmed outcome; it is only meaningful on success, because the caller
// logs the outcome after the call returns.
type PlacedBid struct {
	ID            int64
	LotID         int64
	ParticipantID int64
	Amount        int64
	AcceptedAt    time.Time
	RequestKey    string
	Repeated      bool
	Duration      time.Duration
}

// PlaceBid accepts one bid for an active lot inside a single transaction.
// Every way of sending a bid — the HTML form and the JSON API alike — goes
// through this operation, so there is exactly one path that writes bids.
//
// The transaction pins READ COMMITTED: every statement after the granted lot
// lock must see what the previous lock holder committed, which turns
// concurrent retries of one key into a stored repeat instead of a unique
// violation. The work runs in the fixed order: the lot row is locked first
// (a missing or draft lot is answered as ErrNotFound, never revealing the
// draft), then the request key is looked up — the same amount returns the
// stored bid without any state or price check, a different amount is
// ErrRequestKeyConflict — and only a new amount reaches the status and price
// checks. The price comparison never builds max+1, so the int64 limit cannot
// overflow. The final statement computes the running PostgreSQL clock once
// and inserts the bid only when that instant is strictly before the deadline;
// zero inserted rows mean the bidding is closed by time.
//
// The caller bounds the operation with ctx, which also caps the wait for the
// row lock; on every error path the rollback releases the connection and
// nothing is written. A commit failure leaves the outcome unknown — the bid
// may or may not be stored — so the caller must not report a refusal and must
// offer a retry with the same request key.
func (s *Service) PlaceBid(ctx context.Context, participantID, lotID int64, amount int64, requestKey string) (PlacedBid, error) {
	started := time.Now()
	switch {
	case participantID <= 0:
		return PlacedBid{}, ErrParticipantRequired
	case amount <= 0:
		return PlacedBid{}, ErrBidAmountInvalid
	case !validRequestKey(requestKey):
		return PlacedBid{}, ErrRequestKeyInvalid
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PlacedBid{}, fmt.Errorf("begin bid transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var startPrice int64
	var status string
	var endsAt time.Time
	err = tx.QueryRow(ctx, lockLotForBidSQL, lotID).Scan(&startPrice, &status, &endsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlacedBid{}, ErrNotFound
	}
	if err != nil {
		return PlacedBid{}, fmt.Errorf("lock lot for bid: %w", err)
	}

	// The repeat is looked up before the state and the deadline: returning a
	// stored bid is the reply to the old attempt, not a new bid, so even a
	// finished lot answers it.
	var storedID, storedAmount int64
	var storedAcceptedAt time.Time
	err = tx.QueryRow(ctx, selectBidByKeySQL, participantID, lotID, requestKey).
		Scan(&storedID, &storedAmount, &storedAcceptedAt)
	if err == nil {
		if storedAmount != amount {
			return PlacedBid{}, ErrRequestKeyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return PlacedBid{}, fmt.Errorf("commit bid transaction: %w", err)
		}

		return PlacedBid{
			ID:            storedID,
			LotID:         lotID,
			ParticipantID: participantID,
			Amount:        storedAmount,
			AcceptedAt:    storedAcceptedAt,
			RequestKey:    requestKey,
			Repeated:      true,
			Duration:      time.Since(started),
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PlacedBid{}, fmt.Errorf("find bid by request key: %w", err)
	}

	if status != StatusActive {
		return PlacedBid{}, ErrBiddingClosed
	}

	var maxAmount int64
	if err := tx.QueryRow(ctx, selectMaxBidSQL, lotID).Scan(&maxAmount); err != nil {
		return PlacedBid{}, fmt.Errorf("measure maximal bid: %w", err)
	}
	// The first accepted bid may equal the start price; every next bid must
	// be strictly above the maximal accepted amount. Bids are positive (the
	// CHECK constraint), so a zero maximum means no bids yet. Comparing
	// without max+1 keeps the int64 limit safe.
	if maxAmount == 0 && amount < startPrice || maxAmount > 0 && amount <= maxAmount {
		return PlacedBid{}, ErrBidTooLow
	}

	var bidID int64
	var acceptedAt time.Time
	err = tx.QueryRow(ctx, insertBidSQL, lotID, participantID, amount, requestKey, endsAt).
		Scan(&bidID, &acceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlacedBid{}, ErrBiddingClosed
	}
	if err != nil {
		if mapped := mapConstraintError(err); mapped != nil {
			return PlacedBid{}, mapped
		}

		return PlacedBid{}, fmt.Errorf("insert bid: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return PlacedBid{}, fmt.Errorf("commit bid transaction: %w", err)
	}

	return PlacedBid{
		ID:            bidID,
		LotID:         lotID,
		ParticipantID: participantID,
		Amount:        amount,
		AcceptedAt:    acceptedAt,
		RequestKey:    requestKey,
		Duration:      time.Since(started),
	}, nil
}

// validRequestKey reports whether the key is a canonical hyphenated UUID
// (8-4-4-4-12 hex digits, any case): the format the forms and the API issue,
// and the exact shape the unique constraint matches.
func validRequestKey(key string) bool {
	if len(key) != 36 {
		return false
	}
	for i := 0; i < len(key); i++ {
		switch i {
		case 8, 13, 18, 23:
			if key[i] != '-' {
				return false
			}
		default:
			c := key[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}

	return true
}
