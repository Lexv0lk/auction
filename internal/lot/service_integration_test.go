//go:build integration

package lot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/testutil"
)

// createCategory inserts a category directly and returns its ID.
func createCategory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(ctx, "INSERT INTO categories (name) VALUES ($1) RETURNING id", name).Scan(&id)
	require.NoError(t, err)

	return id
}

// lotRow is the raw lot table record, read directly and bypassing the service.
type lotRow struct {
	title       string
	description string
	categoryID  int64
	startPrice  int64
	status      string
	endsAt      time.Time
}

func loadLotRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (lotRow, bool) {
	t.Helper()

	var row lotRow
	err := pool.QueryRow(ctx,
		"SELECT title, description, category_id, start_price, status, ends_at FROM lots WHERE id = $1", id).
		Scan(&row.title, &row.description, &row.categoryID, &row.startPrice, &row.status, &row.endsAt)
	if err != nil {
		return lotRow{}, false
	}
	require.NoError(t, err)

	return row, true
}

func TestLotCRUDAndPublicationAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	coins := createCategory(t, ctx, pool, "Нумизматика")
	books := createCategory(t, ctx, pool, "Антикварные книги")
	endsAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	// Create: the draft is stored with the trimmed strings and the absolute
	// deadline.
	created, err := service.Create(ctx, Input{
		Title:       "  Серебряный рубль 1726 года ",
		Description: "Монета в хорошем состоянии",
		CategoryID:  coins,
		StartPrice:  5000,
		EndsAt:      endsAt,
	})
	require.NoError(t, err)
	assert.Positive(t, created.ID)
	assert.Equal(t, StatusDraft, created.Status)

	stored, ok := loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok, "the draft is in the database")
	assert.Equal(t, "Серебряный рубль 1726 года", stored.title)
	assert.Equal(t, coins, stored.categoryID)
	assert.Equal(t, int64(5000), stored.startPrice)
	assert.Equal(t, StatusDraft, stored.status)
	assert.True(t, endsAt.Equal(stored.endsAt), "the deadline is stored as absolute time")

	// Get returns the stored lot with its category name.
	got, err := service.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "Серебряный рубль 1726 года", got.Title)
	assert.Equal(t, "Нумизматика", got.CategoryName)
	assert.Equal(t, StatusDraft, got.Status)

	// Update rewrites every editable field of the draft.
	updatedInput := Input{
		Title:       "Серебряный рубль, рейтинг NGC",
		Description: "Монета в слабе, грейд MS62",
		CategoryID:  books,
		StartPrice:  7500,
		EndsAt:      endsAt.Add(24 * time.Hour),
	}
	updated, err := service.Update(ctx, created.ID, updatedInput)
	require.NoError(t, err)
	assert.Equal(t, StatusDraft, updated.Status, "the update never changes the status")

	stored, ok = loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok)
	assert.Equal(t, updatedInput.Title, stored.title)
	assert.Equal(t, updatedInput.Description, stored.description)
	assert.Equal(t, books, stored.categoryID)
	assert.Equal(t, updatedInput.StartPrice, stored.startPrice)
	assert.True(t, updatedInput.EndsAt.Equal(stored.endsAt))
	assert.Equal(t, StatusDraft, stored.status)

	// Publication opens the bidding and freezes the conditions.
	published, err := service.Publish(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusActive, published.Status)
	assert.True(t, updatedInput.EndsAt.Equal(published.EndsAt), "publication never moves the deadline")

	stored, ok = loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok)
	assert.Equal(t, StatusActive, stored.status)
	assert.True(t, updatedInput.EndsAt.Equal(stored.endsAt), "the published deadline is unchanged")

	// After publication every condition-changing operation is refused.
	_, err = service.Update(ctx, created.ID, Input{
		Title: "Взлом", Description: "Описание", CategoryID: coins, StartPrice: 1, EndsAt: endsAt,
	})
	assert.ErrorIs(t, err, ErrNotDraft, "editing an active lot is refused")
	err = service.Delete(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotDraft, "deleting an active lot is refused")

	// Republication is a conflict and never extends the deadline.
	_, err = service.Publish(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotDraft)
	stored, ok = loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok)
	assert.True(t, updatedInput.EndsAt.Equal(stored.endsAt), "republication does not move the deadline")
	assert.Equal(t, StatusActive, stored.status)

	// The finished state is refused just like the active one.
	_, err = pool.Exec(ctx, "UPDATE lots SET status = 'finished', finished_at = now() WHERE id = $1", created.ID)
	require.NoError(t, err)
	_, err = service.Publish(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotDraft)
	_, err = service.Update(ctx, created.ID, updatedInput)
	assert.ErrorIs(t, err, ErrNotDraft)
	err = service.Delete(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotDraft)
}

func TestDeleteRemovesDraft(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Филателия")

	created, err := service.Create(ctx, Input{
		Title: "Земская марка", Description: "Марка без клея",
		CategoryID: categoryID, StartPrice: 1200,
		EndsAt: time.Now().Add(72 * time.Hour),
	})
	require.NoError(t, err)

	require.NoError(t, service.Delete(ctx, created.ID))
	_, ok := loadLotRow(t, ctx, pool, created.ID)
	assert.False(t, ok, "the draft is gone from the database")

	_, err = service.Get(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotFound)

	// Deleting again is a missing lot, not a success.
	err = service.Delete(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPublishRejectsPastDeadline(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")

	// A draft with a past deadline is creatable and editable; only the
	// publication refuses it and the draft stays.
	created, err := service.Create(ctx, Input{
		Title: "Просроченный черновик", Description: "Дедлайн в прошлом",
		CategoryID: categoryID, StartPrice: 100,
		EndsAt: time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	_, err = service.Publish(ctx, created.ID)
	assert.ErrorIs(t, err, ErrDeadlinePast)

	stored, ok := loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok)
	assert.Equal(t, StatusDraft, stored.status, "the lot stays a draft after the refused publication")

	// Fixing the deadline makes the same draft publishable.
	_, err = service.Update(ctx, created.ID, Input{
		Title: "Просроченный черновик", Description: "Дедлайн в прошлом",
		CategoryID: categoryID, StartPrice: 100,
		EndsAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	_, err = service.Publish(ctx, created.ID)
	assert.NoError(t, err, "the repaired draft publishes")
}

func TestCreateRejectsUnknownCategory(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	createCategory(t, ctx, pool, "Нумизматика")

	_, err := service.Create(ctx, Input{
		Title: "Рубль", Description: "Описание",
		CategoryID: 999999, StartPrice: 100,
		EndsAt: time.Now().Add(time.Hour),
	})
	assert.ErrorIs(t, err, ErrCategoryMissing, "the foreign key rejects the unknown category")

	var lots int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots").Scan(&lots))
	assert.Zero(t, lots, "the refused create leaves no rows")
}

func TestListReturnsEveryStatus(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Нумизматика")

	first, err := service.Create(ctx, Input{
		Title: "Старый лот", Description: "Описание",
		CategoryID: categoryID, StartPrice: 100, EndsAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	_, err = service.Create(ctx, Input{
		Title: "Новый лот", Description: "Описание",
		CategoryID: categoryID, StartPrice: 200, EndsAt: time.Now().Add(2 * time.Hour),
	})
	require.NoError(t, err)
	_, err = service.Publish(ctx, first.ID)
	require.NoError(t, err)

	lots, err := service.List(ctx)
	require.NoError(t, err)
	require.Len(t, lots, 2)
	assert.Equal(t, "Новый лот", lots[0].Title, "the list is newest first")
	assert.Equal(t, StatusDraft, lots[0].Status)
	assert.Equal(t, StatusActive, lots[1].Status)
	assert.Equal(t, "Нумизматика", lots[1].CategoryName)
}

func TestUpdateRacesPublication(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)
	categoryID := createCategory(t, ctx, pool, "Живопись и графика")

	const originalTitle = "Акварель с видом на залив"
	const editedTitle = "Акварель, обновлённое описание"
	// TIMESTAMPTZ keeps microseconds, so the expected deadline is truncated.
	endsAt := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	created, err := service.Create(ctx, Input{
		Title: originalTitle, Description: "Акварель неизвестного художника",
		CategoryID: categoryID, StartPrice: 2500, EndsAt: endsAt,
	})
	require.NoError(t, err)

	// The update and the publication start together; the row lock resolves
	// the race into one consistent version: either the publication opens the
	// bidding with the edited conditions, or the edit arrives after the
	// publication and is refused.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var updateErr, publishErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, updateErr = service.Update(ctx, created.ID, Input{
			Title: editedTitle, Description: "Акварель неизвестного художника",
			CategoryID: categoryID, StartPrice: 2500, EndsAt: endsAt,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, publishErr = service.Publish(ctx, created.ID)
	}()
	close(start)
	wg.Wait()

	require.NoError(t, publishErr, "the publication never fails against a draft")
	stored, ok := loadLotRow(t, ctx, pool, created.ID)
	require.True(t, ok)
	assert.Equal(t, StatusActive, stored.status)
	assert.True(t, endsAt.Equal(stored.endsAt), "the race never moves the deadline")

	if updateErr == nil {
		assert.Equal(t, editedTitle, stored.title, "the edit won: the publication opened the bidding with the edited conditions")
	} else {
		assert.ErrorIs(t, updateErr, ErrNotDraft, "the publication won: the late edit is refused")
		assert.Equal(t, originalTitle, stored.title, "the refused edit changed nothing")
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}
