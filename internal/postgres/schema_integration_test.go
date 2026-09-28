//go:build integration

package postgres_test

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/postgres"
	"github.com/Lexv0lk/auction/internal/testutil"
)

func TestSchemaVersionMatchesBinary(t *testing.T) {
	pool := testutil.Pool(t)

	require.NoError(t, postgres.CheckSchemaVersion(testCtx(t), pool))
}

func TestUserConstraints(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	mustExec(t, ctx, pool, "INSERT INTO users (login, password_hash, role) VALUES ('admin', 'hash', 'admin')")
	mustExec(t, ctx, pool, "INSERT INTO users (login, password_hash, role) VALUES ('user_one', 'hash', 'participant')")

	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO users (login, password_hash, role) VALUES ('moderator', 'hash', 'moderator')"),
		"unknown roles are rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO users (login, password_hash, role) VALUES ('', 'hash', 'participant')"),
		"empty logins are rejected")
	assert.Equal(t, "23505", execErrorCode(t, ctx, pool,
		"INSERT INTO users (login, password_hash, role) VALUES ('admin', 'hash', 'participant')"),
		"duplicate logins are rejected")
}

func TestCategoryConstraints(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	mustExec(t, ctx, pool, "INSERT INTO categories (name) VALUES ('Coins')")
	mustExec(t, ctx, pool, "INSERT INTO categories (name) VALUES ('Paintings')")

	assert.Equal(t, "23514", execErrorCode(t, ctx, pool, "INSERT INTO categories (name) VALUES ('')"),
		"empty names are rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO categories (name) VALUES (repeat('x', 121))"),
		"names longer than 120 characters are rejected")
	assert.Equal(t, "23505", execErrorCode(t, ctx, pool, "INSERT INTO categories (name) VALUES ('coins')"),
		"normalized (case-insensitive) names must stay unique")
	assert.Equal(t, "23505", execErrorCode(t, ctx, pool, "INSERT INTO categories (name) VALUES ('paintings')"),
		"normalized (case-insensitive) names must stay unique")
}

func TestLotConstraints(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := insertCategory(t, ctx, pool, "Coins")

	assert.Equal(t, "23503", execErrorCode(t, ctx, pool, insertLotSQL(), 9999),
		"unknown categories are rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at)"+
			" VALUES ('Lot', 'Description', $1, 100, 'closed', now() + interval '1 hour')", categoryID),
		"unknown statuses are rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at, winning_bid_id)"+
			" VALUES ('Lot', 'Description', $1, 100, 'active', now() + interval '1 hour', 1)", categoryID),
		"an active lot must not carry a result yet")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at)"+
			" VALUES ('Lot', 'Description', $1, 0, 'draft', now() + interval '1 hour')", categoryID),
		"zero start price is rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at)"+
			" VALUES ('Lot', 'Description', $1, -5, 'draft', now() + interval '1 hour')", categoryID),
		"negative start price is rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at)"+
			" VALUES ('', 'Description', $1, 100, 'draft', now() + interval '1 hour')", categoryID),
		"empty titles are rejected")
}

func TestLotResultStates(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := insertCategory(t, ctx, pool, "Coins")
	lotID := insertLot(t, ctx, pool, categoryID)
	bidID := insertBid(t, ctx, pool, lotID, insertUser(t, ctx, pool, "user_one", "participant"), 100, 1)

	otherLotID := insertLot(t, ctx, pool, categoryID)
	assert.Equal(t, "23503", execErrorCode(t, ctx, pool,
		"UPDATE lots SET status = 'finished', finished_at = now(), winning_bid_id = $1 WHERE id = $2",
		bidID, otherLotID),
		"a winning bid of another lot is rejected")

	mustExec(t, ctx, pool,
		"UPDATE lots SET status = 'finished', finished_at = now(), winning_bid_id = $1 WHERE id = $2",
		bidID, lotID)
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"UPDATE lots SET finished_at = NULL WHERE id = $1", lotID),
		"finished lots must keep finished_at")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool,
		"UPDATE lots SET winning_bid_id = $1 WHERE id = $2", bidID, insertLot(t, ctx, pool, categoryID)),
		"draft and active lots must not carry a winner")

	// finished without bids is allowed and leaves the winner NULL.
	finishedLotID := insertLot(t, ctx, pool, categoryID)
	mustExec(t, ctx, pool, "UPDATE lots SET status = 'finished', finished_at = now() WHERE id = $1", finishedLotID)

	var winner *int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT winning_bid_id FROM lots WHERE id = $1", finishedLotID).Scan(&winner))
	assert.Nil(t, winner, "finished lots without bids have no winner")
}

func TestBidConstraints(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := insertCategory(t, ctx, pool, "Coins")
	lotID := insertLot(t, ctx, pool, categoryID)
	userOne := insertUser(t, ctx, pool, "user_one", "participant")
	userTwo := insertUser(t, ctx, pool, "user_two", "participant")

	assert.Equal(t, "23514", execErrorCode(t, ctx, pool, insertBidSQL(), lotID, userOne, 0, requestKey(1)),
		"zero amounts are rejected")
	assert.Equal(t, "23514", execErrorCode(t, ctx, pool, insertBidSQL(), lotID, userOne, -100, requestKey(1)),
		"negative amounts are rejected")
	assert.Equal(t, "23503", execErrorCode(t, ctx, pool, insertBidSQL(), 9999, userOne, 100, requestKey(1)),
		"unknown lots are rejected")
	assert.Equal(t, "23503", execErrorCode(t, ctx, pool, insertBidSQL(), lotID, 9999, 100, requestKey(1)),
		"unknown users are rejected")

	insertBid(t, ctx, pool, lotID, userOne, 100, 1)
	assert.Equal(t, "23505", execErrorCode(t, ctx, pool, insertBidSQL(), lotID, userOne, 150, requestKey(1)),
		"a repeated request key per participant and lot is rejected")

	// The same key is allowed for other participants and other lots.
	insertBid(t, ctx, pool, lotID, userTwo, 100, 1)
	otherLotID := insertLot(t, ctx, pool, categoryID)
	insertBid(t, ctx, pool, otherLotID, userOne, 100, 1)

	// Equal amounts do not conflict at the schema level; the order of
	// acceptance is the service layer's job (step 09).
	insertBid(t, ctx, pool, lotID, userTwo, 200, 2)

	var bidsOnLot int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM bids WHERE lot_id = $1", lotID).Scan(&bidsOnLot))
	assert.Equal(t, 3, bidsOnLot)
}

func TestDeleteProtection(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := insertCategory(t, ctx, pool, "Coins")
	unusedCategoryID := insertCategory(t, ctx, pool, "Paintings")
	lotID := insertLot(t, ctx, pool, categoryID)
	userID := insertUser(t, ctx, pool, "user_one", "participant")
	insertBid(t, ctx, pool, lotID, userID, 100, 1)

	assert.Equal(t, "23001", execErrorCode(t, ctx, pool, "DELETE FROM categories WHERE id = $1", categoryID),
		"a used category is not deleted")
	assert.Equal(t, "23001", execErrorCode(t, ctx, pool, "DELETE FROM lots WHERE id = $1", lotID),
		"a lot with bids is not deleted")
	assert.Equal(t, "23001", execErrorCode(t, ctx, pool, "DELETE FROM users WHERE id = $1", userID),
		"a user with bids is not deleted")

	mustExec(t, ctx, pool, "DELETE FROM categories WHERE id = $1", unusedCategoryID)

	var bidsLeft int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM bids").Scan(&bidsLeft))
	assert.Equal(t, 1, bidsLeft, "bid history is never removed together with its owners")
}

func TestRequiredIndexesExist(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	expected := map[string]string{
		"sessions_expires_at_idx":   "",
		"sessions_user_id_idx":      "",
		"categories_name_lower_idx": "lower",
		"lots_active_ends_at_idx":   "active",
		"lots_category_id_idx":      "",
		"bids_lot_amount_idx":       "amount",
		"bids_lot_history_idx":      "accepted_at",
	}

	rows, err := pool.Query(ctx, "SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = 'public'")
	require.NoError(t, err)
	defer rows.Close()

	found := make(map[string]string)
	for rows.Next() {
		var name, definition string
		require.NoError(t, rows.Scan(&name, &definition))
		found[name] = definition
	}
	require.NoError(t, rows.Err())

	for name, fragment := range expected {
		definition, ok := found[name]
		require.True(t, ok, "index %s is missing", name)
		if fragment != "" {
			assert.Contains(t, definition, fragment, "index %s must match its purpose", name)
		}
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	_, err := pool.Exec(ctx, sql, args...)
	require.NoError(t, err, "statement %q", sql)
}

// execErrorCode executes a statement that must fail and returns the PostgreSQL
// error code (23514 check violation, 23505 unique violation, 23503 FK violation,
// 23001 restrict violation).
func execErrorCode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()

	_, err := pool.Exec(ctx, sql, args...)
	require.Error(t, err, "statement %q must fail", sql)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)

	return pgErr.Code
}

func insertUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, login, role string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		"INSERT INTO users (login, password_hash, role) VALUES ($1, 'hash', $2) RETURNING id",
		login, role).Scan(&id))

	return id
}

func insertCategory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ($1) RETURNING id", name).Scan(&id))

	return id
}

// insertLotSQL prepares an active lot insert; the caller passes categoryID
// as the first and only query argument.
func insertLotSQL() string {
	return "INSERT INTO lots (title, description, category_id, start_price, status, ends_at)" +
		" VALUES ('Lot', 'Description', $1, 100, 'active', now() + interval '1 hour')"
}

func insertLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, categoryID int64) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx, insertLotSQL()+" RETURNING id", categoryID).Scan(&id))

	return id
}

// insertBidSQL prepares a bid insert; the caller passes lotID, userID, amount
// and the request key as query arguments.
func insertBidSQL() string {
	return "INSERT INTO bids (lot_id, user_id, amount, accepted_at, request_key)" +
		" VALUES ($1, $2, $3, now(), $4)"
}

func insertBid(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lotID, userID, amount int64, key uint64) int64 {
	t.Helper()

	var id int64
	require.NoError(t, pool.QueryRow(ctx, insertBidSQL()+" RETURNING id",
		lotID, userID, amount, requestKey(key)).Scan(&id))

	return id
}

// requestKey builds a deterministic UUID request key for tests.
func requestKey(value uint64) pgtype.UUID {
	var key pgtype.UUID
	binary.BigEndian.PutUint64(key.Bytes[:8], value)
	key.Valid = true

	return key
}
