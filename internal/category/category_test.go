package category

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errFakeRowDestination = errors.New("fakeRow: unsupported destination")
	errFakeMustNotQuery   = errors.New("fake pool: this query must not run")
	errFakeConnectionDown = errors.New("connection refused")
)

func TestNormalizeName(t *testing.T) {
	assert.Equal(t, "Нумизматика", NormalizeName("  Нумизматика\t"))
	assert.Equal(t, "", NormalizeName("   "))
}

func TestValidateName(t *testing.T) {
	assert.ErrorIs(t, ValidateName(""), ErrNameEmpty)
	assert.ErrorIs(t, ValidateName("   "), ErrNameEmpty)
	assert.ErrorIs(t, ValidateName(strings.Repeat("а", MaxNameLength+1)), ErrNameTooLong)
	assert.NoError(t, ValidateName(strings.Repeat("а", MaxNameLength)))
}

func TestCreateNormalizesName(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{int64(1), "Нумизматика"}}}
	service := NewService(pool)

	created, err := service.Create(context.Background(), "  Нумизматика  ")
	require.NoError(t, err)
	assert.Equal(t, Category{ID: 1, Name: "Нумизматика"}, created)

	require.Len(t, pool.rowCalls, 1)
	require.Len(t, pool.rowCalls[0].args, 1)
	assert.Equal(t, "Нумизматика", pool.rowCalls[0].args[0], "the insert receives the normalized display name")
}

func TestCreateRejectsInvalidNamesBeforeDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		err  error
	}{
		{"empty", "   ", ErrNameEmpty},
		{"too long", strings.Repeat("а", MaxNameLength+1), ErrNameTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &fakePool{row: fakeRow{err: errFakeMustNotQuery}}
			service := NewService(pool)

			_, err := service.Create(context.Background(), tc.raw)
			assert.ErrorIs(t, err, tc.err)
			assert.Empty(t, pool.rowCalls, "invalid names must be rejected before the database")
		})
	}
}

func TestCreateMapsUniqueViolationToErrExists(t *testing.T) {
	pool := &fakePool{row: fakeRow{err: &pgconn.PgError{Code: pgErrUniqueViolation, ConstraintName: "categories_name_lower_idx"}}}
	service := NewService(pool)

	_, err := service.Create(context.Background(), "Нумизматика")
	assert.ErrorIs(t, err, ErrExists)

	pool = &fakePool{row: fakeRow{err: &pgconn.PgError{Code: pgErrUniqueViolation, ConstraintName: "unrelated_index"}}}
	service = NewService(pool)
	_, err = service.Create(context.Background(), "Нумизматика")
	assert.NotErrorIs(t, err, ErrExists, "unique violations of other constraints stay technical errors")

	pool = &fakePool{row: fakeRow{err: errFakeConnectionDown}}
	service = NewService(pool)
	_, err = service.Create(context.Background(), "Нумизматика")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExists)
}

func TestGetReturnsCategoryOrNotFound(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{int64(3), "Филателия"}}}
	service := NewService(pool)

	loaded, err := service.Get(context.Background(), 3)
	require.NoError(t, err)
	assert.Equal(t, Category{ID: 3, Name: "Филателия"}, loaded)
	require.Len(t, pool.rowCalls, 1)
	require.Len(t, pool.rowCalls[0].args, 1)
	assert.Equal(t, int64(3), pool.rowCalls[0].args[0])

	pool = &fakePool{row: fakeRow{err: pgx.ErrNoRows}}
	service = NewService(pool)
	_, err = service.Get(context.Background(), 404)
	assert.ErrorIs(t, err, ErrNotFound)

	pool = &fakePool{row: fakeRow{err: errFakeConnectionDown}}
	service = NewService(pool)
	_, err = service.Get(context.Background(), 1)
	assert.NotErrorIs(t, err, ErrNotFound, "a database failure must not look like a missing category")
}

func TestRenameValidatesMapsNotFoundAndDuplicates(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{int64(3), "Боны"}}}
	service := NewService(pool)

	renamed, err := service.Rename(context.Background(), 3, "  Боны ")
	require.NoError(t, err)
	assert.Equal(t, Category{ID: 3, Name: "Боны"}, renamed)
	require.Len(t, pool.rowCalls, 1)
	require.Len(t, pool.rowCalls[0].args, 2)
	assert.Equal(t, int64(3), pool.rowCalls[0].args[0])
	assert.Equal(t, "Боны", pool.rowCalls[0].args[1])

	pool = &fakePool{row: fakeRow{err: pgx.ErrNoRows}}
	service = NewService(pool)
	_, err = service.Rename(context.Background(), 404, "Боны")
	assert.ErrorIs(t, err, ErrNotFound)

	pool = &fakePool{row: fakeRow{err: &pgconn.PgError{Code: pgErrUniqueViolation, ConstraintName: "categories_name_lower_idx"}}}
	service = NewService(pool)
	_, err = service.Rename(context.Background(), 3, "Нумизматика")
	assert.ErrorIs(t, err, ErrExists)

	pool = &fakePool{row: fakeRow{err: errFakeMustNotQuery}}
	service = NewService(pool)
	_, err = service.Rename(context.Background(), 3, " ")
	assert.ErrorIs(t, err, ErrNameEmpty)
	assert.Empty(t, pool.rowCalls, "invalid names must be rejected before the database")
}

func TestListReadsAllCategories(t *testing.T) {
	pool := &fakePool{rows: newFakeRows(
		[]any{int64(2), "Антикварные книги"},
		[]any{int64(1), "Нумизматика"},
	)}
	service := NewService(pool)

	categories, err := service.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Category{
		{ID: 2, Name: "Антикварные книги"},
		{ID: 1, Name: "Нумизматика"},
	}, categories)

	pool = &fakePool{rowsErr: errFakeConnectionDown}
	service = NewService(pool)
	_, err = service.List(context.Background())
	assert.Error(t, err)
}

func TestDeleteChecksUsageThenDeletes(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{false}}, deleteRow: fakeRow{values: []any{int64(5)}}}
	service := NewService(pool)

	err := service.Delete(context.Background(), 5)
	require.NoError(t, err)
	require.Len(t, pool.rowCalls, 2, "the usage pre-check runs first")
	assert.NotContains(t, pool.rowCalls[0].sql, "DELETE", "the pre-check only reads")
	assert.Contains(t, pool.rowCalls[1].sql, "DELETE FROM categories")
	require.Len(t, pool.rowCalls[1].args, 1)
	assert.Equal(t, int64(5), pool.rowCalls[1].args[0])
}

func TestDeleteRefusesUsedCategoryWithoutDeleteStatement(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{true}}, deleteRow: fakeRow{err: errFakeMustNotQuery}}
	service := NewService(pool)

	err := service.Delete(context.Background(), 5)
	assert.ErrorIs(t, err, ErrInUse)
	require.Len(t, pool.rowCalls, 1, "an in-use category must not be deleted")
}

func TestDeleteMapsForeignKeyViolationToErrInUse(t *testing.T) {
	// The FK is the final guard against a lot created between the pre-check
	// and the delete; its violation converts to the same user-visible error.
	pool := &fakePool{
		row: fakeRow{values: []any{false}},
		deleteRow: fakeRow{err: &pgconn.PgError{
			Code:           pgErrForeignKeyViolation,
			ConstraintName: "lots_category_id_fk",
		}},
	}
	service := NewService(pool)

	err := service.Delete(context.Background(), 5)
	assert.ErrorIs(t, err, ErrInUse)

	pool = &fakePool{row: fakeRow{values: []any{false}}, deleteRow: fakeRow{err: errFakeConnectionDown}}
	service = NewService(pool)
	err = service.Delete(context.Background(), 5)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInUse)
}

func TestDeleteMissingCategoryIsNotFound(t *testing.T) {
	pool := &fakePool{row: fakeRow{values: []any{false}}, deleteRow: fakeRow{err: pgx.ErrNoRows}}
	service := NewService(pool)

	err := service.Delete(context.Background(), 404)
	assert.ErrorIs(t, err, ErrNotFound, "deleting an absent category is 404, not a silent success")
}
