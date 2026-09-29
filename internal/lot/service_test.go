package lot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errDatabaseDown = errors.New("fake database down")
	errBeginFailed  = errors.New("begin failed")
	errCommitFailed = errors.New("commit failed")
)

func validInput() Input {
	return NormalizeInput(Input{
		Title:       "  Серебряный рубль  ",
		Description: "Монета в хорошем состоянии",
		CategoryID:  3,
		StartPrice:  5000,
		EndsAt:      time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
	})
}

func fkError() *pgconn.PgError {
	return &pgconn.PgError{Code: "23503", ConstraintName: "lots_category_id_fk"}
}

func TestCreateInsertsDraft(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{int64(7), StatusDraft}}}
	service := NewService(pool)
	input := validInput()

	created, err := service.Create(context.Background(), input)
	require.NoError(t, err)

	assert.Equal(t, int64(7), created.ID)
	assert.Equal(t, "Серебряный рубль", created.Title)
	assert.Equal(t, input.Description, created.Description)
	assert.Equal(t, input.CategoryID, created.CategoryID)
	assert.Equal(t, input.StartPrice, created.StartPrice)
	assert.Equal(t, StatusDraft, created.Status)
	assert.True(t, input.EndsAt.Equal(created.EndsAt))

	require.Len(t, pool.queries, 1, "create is a single INSERT, no transaction")
	query := pool.queries[0]
	assert.Contains(t, query.sql, "INSERT INTO lots")
	assert.Contains(t, query.sql, "'draft'")
	require.Len(t, query.args, 5)
	assert.Equal(t, "Серебряный рубль", query.args[0])
	assert.Equal(t, input.CategoryID, query.args[2])
	assert.Equal(t, input.StartPrice, query.args[3])
	assert.True(t, input.EndsAt.Equal(query.args[4].(time.Time)))
}

func TestCreateRejectsInvalidInputWithoutSQL(t *testing.T) {
	pool := &fakePool{}
	service := NewService(pool)

	_, err := service.Create(context.Background(), Input{})
	assert.ErrorIs(t, err, ErrTitleEmpty)
	assert.Empty(t, pool.queries, "validation runs before any SQL")
}

func TestCreateMapsUnknownCategory(t *testing.T) {
	pool := &fakePool{row: fakeRow{err: fkError()}}
	service := NewService(pool)

	_, err := service.Create(context.Background(), validInput())
	assert.ErrorIs(t, err, ErrCategoryMissing)

	pool.row = fakeRow{err: errDatabaseDown}
	_, err = service.Create(context.Background(), validInput())
	assert.NotErrorIs(t, err, ErrCategoryMissing, "technical failures stay wrapped")
}

func TestUpdateRewritesDraftFields(t *testing.T) {
	input := validInput()
	input.Title = "Обновлённый рубль"
	input.CategoryID = 4
	tx := &fakeTx{
		lockRow:   fakeRow{values: []any{int64(5), StatusDraft}},
		changeRow: fakeRow{values: []any{int64(5)}},
	}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	updated, err := service.Update(context.Background(), 5, input)
	require.NoError(t, err)

	assert.Equal(t, int64(5), updated.ID)
	assert.Equal(t, "Обновлённый рубль", updated.Title)
	assert.Equal(t, StatusDraft, updated.Status, "the update never changes the status")

	require.Len(t, tx.queries, 2, "the row is locked first, updated second")
	assert.Contains(t, tx.queries[0].sql, "FOR UPDATE")
	assert.Equal(t, int64(5), tx.queries[0].args[0])
	assert.Contains(t, tx.queries[1].sql, "UPDATE lots SET")
	assert.Equal(t, "Обновлённый рубль", tx.queries[1].args[1])
	assert.Equal(t, int64(4), tx.queries[1].args[3])
	assert.True(t, tx.committed)
}

func TestUpdateRefusesNonDraft(t *testing.T) {
	for _, status := range []string{StatusActive, StatusFinished} {
		tx := &fakeTx{lockRow: fakeRow{values: []any{int64(5), status}}}
		pool := &fakePool{tx: tx}
		service := NewService(pool)

		_, err := service.Update(context.Background(), 5, validInput())
		assert.ErrorIs(t, err, ErrNotDraft, "status %s", status)
		assert.Len(t, tx.queries, 1, "only the locking SELECT ran, no UPDATE")
		assert.True(t, tx.rolledBack)
		assert.False(t, tx.committed)
	}
}

func TestUpdateMissingLot(t *testing.T) {
	tx := &fakeTx{lockRow: fakeRow{err: pgx.ErrNoRows}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.Update(context.Background(), 987654, validInput())
	assert.ErrorIs(t, err, ErrNotFound)
	assert.True(t, tx.rolledBack)
}

func TestUpdateMapsUnknownCategory(t *testing.T) {
	tx := &fakeTx{
		lockRow:   fakeRow{values: []any{int64(5), StatusDraft}},
		changeRow: fakeRow{err: fkError()},
	}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.Update(context.Background(), 5, validInput())
	assert.ErrorIs(t, err, ErrCategoryMissing)
	assert.True(t, tx.rolledBack, "the aborted transaction is rolled back")
}

func TestUpdateRejectsInvalidInputWithoutTransaction(t *testing.T) {
	pool := &fakePool{}
	service := NewService(pool)

	_, err := service.Update(context.Background(), 5, Input{})
	assert.ErrorIs(t, err, ErrTitleEmpty)
	assert.Nil(t, pool.tx, "validation runs before Begin")
}

func TestDeleteRemovesOnlyDrafts(t *testing.T) {
	tx := &fakeTx{
		lockRow:   fakeRow{values: []any{int64(5), StatusDraft}},
		changeRow: fakeRow{values: []any{int64(5)}},
	}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	require.NoError(t, service.Delete(context.Background(), 5))
	require.Len(t, tx.queries, 2)
	assert.Contains(t, tx.queries[0].sql, "FOR UPDATE")
	assert.Contains(t, tx.queries[1].sql, "DELETE FROM lots")
	assert.True(t, tx.committed)
}

func TestDeleteRefusesNonDraft(t *testing.T) {
	tx := &fakeTx{lockRow: fakeRow{values: []any{int64(5), StatusActive}}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	err := service.Delete(context.Background(), 5)
	assert.ErrorIs(t, err, ErrNotDraft)
	assert.Len(t, tx.queries, 1, "the active lot is locked but never deleted")
	assert.True(t, tx.rolledBack)
}

func TestDeleteMissingLot(t *testing.T) {
	tx := &fakeTx{lockRow: fakeRow{err: pgx.ErrNoRows}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	err := service.Delete(context.Background(), 987654)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPublishActivatesCompleteDraft(t *testing.T) {
	endsAt := time.Now().Add(24 * time.Hour)
	dbNow := time.Now()
	tx := &fakeTx{
		lockRow: fakeRow{values: []any{
			int64(5), "Серебряный рубль", "Описание", int64(3), int64(5000), StatusDraft, endsAt, dbNow,
		}},
		changeRow: fakeRow{values: []any{int64(5)}},
	}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	published, err := service.Publish(context.Background(), 5)
	require.NoError(t, err)

	assert.Equal(t, int64(5), published.ID)
	assert.Equal(t, StatusActive, published.Status)
	assert.Equal(t, "Серебряный рубль", published.Title)
	assert.True(t, endsAt.Equal(published.EndsAt), "publication never moves the deadline")

	require.Len(t, tx.queries, 2)
	assert.Contains(t, tx.queries[0].sql, "FOR UPDATE")
	assert.Contains(t, tx.queries[0].sql, "now()")
	assert.Contains(t, tx.queries[1].sql, "status = 'active'")
	assert.True(t, tx.committed)
}

func TestPublishRefusesNonDraft(t *testing.T) {
	for _, status := range []string{StatusActive, StatusFinished} {
		tx := &fakeTx{lockRow: fakeRow{values: []any{
			int64(5), "Рубль", "Описание", int64(3), int64(5000), status,
			time.Now().Add(24 * time.Hour), time.Now(),
		}}}
		pool := &fakePool{tx: tx}
		service := NewService(pool)

		_, err := service.Publish(context.Background(), 5)
		assert.ErrorIs(t, err, ErrNotDraft, "status %s", status)
		assert.Len(t, tx.queries, 1, "no UPDATE: republishing must never touch the lot")
		assert.True(t, tx.rolledBack)
		assert.False(t, tx.committed)
	}
}

func TestPublishRequiresFutureDeadlineByDatabaseClock(t *testing.T) {
	dbNow := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	past := &fakeTx{lockRow: fakeRow{values: []any{
		int64(5), "Рубль", "Описание", int64(3), int64(5000), StatusDraft,
		dbNow.Add(-time.Second), dbNow,
	}}}
	service := NewService(&fakePool{tx: past})
	_, err := service.Publish(context.Background(), 5)
	assert.ErrorIs(t, err, ErrDeadlinePast)
	assert.Len(t, past.queries, 1, "the past-deadline draft is never updated")

	exact := &fakeTx{lockRow: fakeRow{values: []any{
		int64(5), "Рубль", "Описание", int64(3), int64(5000), StatusDraft, dbNow, dbNow,
	}}}
	service = NewService(&fakePool{tx: exact})
	_, err = service.Publish(context.Background(), 5)
	assert.ErrorIs(t, err, ErrDeadlinePast, "the deadline must be strictly in the future")
}

func TestPublishRequiresCompleteDraft(t *testing.T) {
	// Drafts are complete by construction, but the publication re-checks the
	// stored fields: an empty title can only appear outside the application
	// and must stop the publication.
	tx := &fakeTx{lockRow: fakeRow{values: []any{
		int64(5), "", "Описание", int64(3), int64(5000), StatusDraft,
		time.Now().Add(24 * time.Hour), time.Now(),
	}}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.Publish(context.Background(), 5)
	assert.ErrorIs(t, err, ErrNotPublishable)
	assert.Len(t, tx.queries, 1)
}

func TestPublishMissingLot(t *testing.T) {
	tx := &fakeTx{lockRow: fakeRow{err: pgx.ErrNoRows}}
	pool := &fakePool{tx: tx}
	service := NewService(pool)

	_, err := service.Publish(context.Background(), 987654)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTransactionFailuresPropagate(t *testing.T) {
	beginPool := &fakePool{beginErr: errBeginFailed}
	service := NewService(beginPool)
	_, err := service.Publish(context.Background(), 5)
	assert.Error(t, err)
	assert.Nil(t, beginPool.tx, "Begin failure leaves no transaction to use")

	commitPool := &fakePool{tx: &fakeTx{
		lockRow:   fakeRow{values: []any{int64(5), StatusDraft}},
		changeRow: fakeRow{values: []any{int64(5)}},
		commitErr: errCommitFailed,
	}}
	service = NewService(commitPool)
	err = service.Delete(context.Background(), 5)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "commit"), "commit failures are reported, not swallowed")
}

func TestGetLoadsCategoryName(t *testing.T) {
	endsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	pool := &fakePool{row: fakeRow{values: []any{
		int64(5), "Серебряный рубль", "Описание", int64(3), "Нумизматика", int64(5000), StatusDraft, endsAt,
	}}}
	service := NewService(pool)

	got, err := service.Get(context.Background(), 5)
	require.NoError(t, err)
	assert.Equal(t, Lot{
		ID: 5, Title: "Серебряный рубль", Description: "Описание",
		CategoryID: 3, CategoryName: "Нумизматика",
		StartPrice: 5000, Status: StatusDraft, EndsAt: endsAt,
	}, got)

	pool.row = fakeRow{err: pgx.ErrNoRows}
	_, err = service.Get(context.Background(), 987654)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListLoadsAllStatuses(t *testing.T) {
	older := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	pool := &fakePool{rows: newFakeRows(
		[]any{int64(2), "Марка", "Описание", int64(1), "Филателия", int64(1200), StatusActive, newer},
		[]any{int64(1), "Рубль", "Описание", int64(3), "Нумизматика", int64(5000), StatusDraft, older},
	)}
	service := NewService(pool)

	lots, err := service.List(context.Background())
	require.NoError(t, err)
	require.Len(t, lots, 2)
	assert.Equal(t, "Филателия", lots[0].CategoryName)
	assert.Equal(t, StatusActive, lots[0].Status)
	assert.Equal(t, StatusDraft, lots[1].Status)
}
