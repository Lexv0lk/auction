//go:build integration

package category

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/testutil"
)

const insertLotSQL = "INSERT INTO lots (title, description, category_id, start_price, status, ends_at)" +
	" VALUES ($1, 'Учебный лот интеграционного теста', $2, 100, 'draft', now() + make_interval(days => 7))"

// categoryExists checks the table directly, bypassing the service.
func categoryExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) bool {
	t.Helper()

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM categories WHERE id = $1)", id).Scan(&exists))

	return exists
}

func TestCategoryCRUDAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	// Create: the stored name is the normalized display name.
	created, err := service.Create(ctx, "  Нумизматика ")
	require.NoError(t, err)
	assert.Positive(t, created.ID)
	assert.Equal(t, "Нумизматика", created.Name)

	// Uniqueness is case-insensitive and ignores surrounding spaces.
	for _, duplicate := range []string{"нумизматика", "НУМИЗМАТИКА", " Нумизматика\t"} {
		_, err := service.Create(ctx, duplicate)
		assert.ErrorIs(t, err, ErrExists, "duplicate %q", duplicate)
	}

	// Get and List return what Create stored.
	loaded, err := service.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created, loaded)

	other, err := service.Create(ctx, strings.Repeat("б", MaxNameLength))
	require.NoError(t, err, "the 120-character limit counts characters, not bytes")
	list, err := service.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	assert.Equal(t, strings.Repeat("б", MaxNameLength), list[0].Name, "the list is ordered by the normalized name")
	assert.Equal(t, "Нумизматика", list[1].Name)

	// Rename keeps the ID and re-applies the same uniqueness rules.
	renamed, err := service.Rename(ctx, created.ID, " Боны и монеты ")
	require.NoError(t, err)
	assert.Equal(t, created.ID, renamed.ID)
	assert.Equal(t, "Боны и монеты", renamed.Name)
	_, err = service.Rename(ctx, created.ID, strings.Repeat("б", MaxNameLength))
	assert.ErrorIs(t, err, ErrExists, "renaming onto an existing name is rejected")
	_, err = service.Rename(ctx, 987654, "Что-то новое")
	assert.ErrorIs(t, err, ErrNotFound)

	// Delete removes only unused categories.
	require.NoError(t, service.Delete(ctx, created.ID))
	assert.False(t, categoryExists(t, ctx, pool, created.ID))
	_, err = service.Get(ctx, created.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, service.Delete(ctx, other.ID))
}

func TestDeleteRefusesCategoryWithLots(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	// The lot fixture stands in for step 07 lot creation.
	created, err := service.Create(ctx, "Филателия")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, insertLotSQL, "Земская марка 1889", created.ID)
	require.NoError(t, err)

	err = service.Delete(ctx, created.ID)
	assert.ErrorIs(t, err, ErrInUse, "a category referenced by a lot must stay")
	assert.True(t, categoryExists(t, ctx, pool, created.ID), "the category survives the refused deletion")

	var lots int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots WHERE category_id = $1", created.ID).Scan(&lots))
	assert.Equal(t, 1, lots, "the lot survives the refused deletion")
}

func TestDeleteRacesConcurrentLotCreation(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	created, err := service.Create(ctx, "Гонка")
	require.NoError(t, err)

	// The lot insert and the category delete start together; the foreign key
	// must keep the reference consistent no matter which statement wins.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var lotErr, deleteErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, lotErr = pool.Exec(ctx, insertLotSQL, "Лот наперегонки", created.ID)
	}()
	go func() {
		defer wg.Done()
		<-start
		deleteErr = service.Delete(ctx, created.ID)
	}()
	close(start)
	wg.Wait()

	if lotErr == nil {
		require.ErrorIs(t, deleteErr, ErrInUse, "the lot holds the reference, the delete must refuse")
		assert.True(t, categoryExists(t, ctx, pool, created.ID), "the referenced category stays")

		return
	}

	var pgErr *pgconn.PgError
	require.ErrorAs(t, lotErr, &pgErr)
	assert.Equal(t, pgErrForeignKeyViolation, pgErr.Code, "the category vanished first, the lot insert hits the foreign key")
	require.NoError(t, deleteErr)
	assert.False(t, categoryExists(t, ctx, pool, created.ID))
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}
