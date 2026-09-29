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

func TestListCatalogSendsFiltersAndDetectsNextPage(t *testing.T) {
	// One extra row proves there is a next page; the returned page itself is
	// trimmed to the fixed size.
	records := make([][]any, CatalogPageSize+1)
	for i := range records {
		endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
		records[i] = []any{
			int64(100 - i), "Лот", int64(3), "Нумизматика", int64(5000), int64(5100),
			DisplayStateActive, endsAt,
		}
	}
	pool := &fakePool{rows: newFakeRows(records...)}
	service := NewService(pool)

	items, hasMore, err := service.Catalog(context.Background(), CatalogFilter{
		CategoryID: 3, State: DisplayStateActive, Page: 2,
	})
	require.NoError(t, err)
	assert.True(t, hasMore, "the extra row means a next page exists")
	require.Len(t, items, CatalogPageSize)

	require.Len(t, pool.queries, 1, "the catalog is one read query")
	query := pool.queries[0]
	assert.Contains(t, query.sql, "status <> 'draft'", "drafts never appear in the catalog")
	assert.Contains(t, query.sql, "now()", "the display state is computed by the database clock")
	require.Len(t, query.args, 4)
	assert.Equal(t, int64(3), query.args[0], "the category filter reaches the SQL")
	assert.Equal(t, DisplayStateActive, query.args[1])
	assert.Equal(t, CatalogPageSize+1, query.args[2], "the service fetches one extra row")
	assert.Equal(t, CatalogPageSize, query.args[3], "page 2 starts after the first page")

	pool.rows = newFakeRows(records[:1]...)
	_, hasMore, err = service.Catalog(context.Background(), CatalogFilter{})
	require.NoError(t, err)
	assert.False(t, hasMore)
}

func TestListCatalogNormalizesFilter(t *testing.T) {
	pool := &fakePool{rows: newFakeRows()}
	service := NewService(pool)

	_, _, err := service.Catalog(context.Background(), CatalogFilter{State: "bogus", Page: -3})
	require.NoError(t, err)

	require.Len(t, pool.queries, 1)
	query := pool.queries[0]
	assert.Equal(t, int64(0), query.args[0], "a missing category filter means all categories")
	assert.Equal(t, "", query.args[1], "an unknown state is ignored, not passed to the SQL")
	assert.Equal(t, 0, query.args[3], "an invalid page number means the first page")
}

func TestGetPublicLotScansFullRow(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	dbNow := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	pool := &fakePool{row: fakeRow{values: []any{
		int64(7), "Серебряный рубль", "Описание", int64(3), "Нумизматика", int64(5000), StatusActive, endsAt,
		int64(6000), int64(6000), int64(42), int64(6000), "demo-participant-1", DisplayStateActive, dbNow,
	}}}
	service := NewService(pool)

	got, err := service.GetPublicLot(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, int64(7), got.ID)
	assert.Equal(t, "Серебряный рубль", got.Title)
	assert.Equal(t, "Описание", got.Description)
	assert.Equal(t, "Нумизматика", got.CategoryName)
	assert.Equal(t, int64(5000), got.StartPrice)
	assert.Equal(t, StatusActive, got.Status)
	assert.True(t, endsAt.Equal(got.EndsAt))
	assert.Equal(t, int64(6000), got.CurrentPrice)
	assert.Equal(t, int64(6000), got.MaxBid)
	require.NotNil(t, got.WinningBidID)
	assert.Equal(t, int64(42), *got.WinningBidID)
	assert.Equal(t, "demo-participant-1", got.WinningParticipant)
	assert.Equal(t, DisplayStateActive, got.State)
	assert.True(t, dbNow.Equal(got.DatabaseNow), "the server time comes from the database clock")

	require.Len(t, pool.queries, 1, "the current state fields come from one statement")
	assert.Contains(t, pool.queries[0].sql, "status <> 'draft'", "a draft is never served here")
	assert.Contains(t, pool.queries[0].sql, "now()", "the state is computed by the database clock")

	// A lot without bids has no winner row at all.
	pool.row = fakeRow{values: []any{
		int64(8), "Марка", "Описание", int64(1), "Филателия", int64(1200), StatusActive, endsAt,
		int64(1200), int64(0), nil, int64(0), "", DisplayStateActive, dbNow,
	}}
	got, err = service.GetPublicLot(context.Background(), 8)
	require.NoError(t, err)
	assert.Nil(t, got.WinningBidID)
	assert.Equal(t, "", got.WinningParticipant)
}

func TestGetPublicLotMissingOrDraft(t *testing.T) {
	pool := &fakePool{row: fakeRow{err: pgx.ErrNoRows}}
	service := NewService(pool)

	_, err := service.GetPublicLot(context.Background(), 987654)
	assert.ErrorIs(t, err, ErrNotFound, "a missing lot and a draft answer the same way")
}

func TestListBidsPaginates(t *testing.T) {
	acceptedAt := time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	records := make([][]any, BidHistoryPageSize+1)
	for i := range records {
		records[i] = []any{int64(100 - i), "demo-participant-1", int64(5000 + i), acceptedAt}
	}
	pool := &fakePool{rows: newFakeRows(records...)}
	service := NewService(pool)

	bids, hasMore, err := service.ListBids(context.Background(), 7, 1)
	require.NoError(t, err)
	assert.True(t, hasMore)
	require.Len(t, bids, BidHistoryPageSize)
	assert.Equal(t, "demo-participant-1", bids[0].Participant)
	assert.True(t, acceptedAt.Equal(bids[0].AcceptedAt))

	require.Len(t, pool.queries, 1)
	query := pool.queries[0]
	assert.Contains(t, query.sql, "ORDER BY b.accepted_at DESC")
	require.Len(t, query.args, 3)
	assert.Equal(t, int64(7), query.args[0])
	assert.Equal(t, BidHistoryPageSize+1, query.args[1], "one extra row detects the next page")
	assert.Equal(t, 0, query.args[2])

	_, _, err = service.ListBids(context.Background(), 7, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, pool.queries[1].args[2], "an invalid page number means the first page")
}

func TestPublicLotMinimumNextBid(t *testing.T) {
	active := PublicLot{StartPrice: 5000, MaxBid: 6000, CurrentPrice: 6000, State: DisplayStateActive}
	minimum, ok := active.MinimumNextBid()
	assert.True(t, ok)
	assert.Equal(t, int64(6001), minimum)

	// Before the first bid the minimum next bid is the start price itself:
	// the first accepted bid may equal it.
	fresh := PublicLot{StartPrice: 5000, MaxBid: 0, CurrentPrice: 5000, State: DisplayStateActive}
	minimum, ok = fresh.MinimumNextBid()
	assert.True(t, ok)
	assert.Equal(t, int64(5000), minimum)

	// The next bid after the maximal int64 amount is not computable: the
	// service never builds the overflowing "current price + 1".
	maxed := PublicLot{StartPrice: 1, MaxBid: math.MaxInt64, CurrentPrice: math.MaxInt64, State: DisplayStateActive}
	_, ok = maxed.MinimumNextBid()
	assert.False(t, ok)
	assert.False(t, maxed.CanBid())

	// Closed states never offer a next bid, whatever the prices are.
	for _, state := range []string{DisplayStateDetermining, DisplayStateFinished} {
		closed := PublicLot{StartPrice: 5000, MaxBid: 6000, CurrentPrice: 6000, State: state}
		_, ok = closed.MinimumNextBid()
		assert.False(t, ok, "state %s", state)
		assert.False(t, closed.CanBid())
	}

	assert.True(t, active.CanBid())
}
