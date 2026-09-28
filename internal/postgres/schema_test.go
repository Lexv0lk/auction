package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/postgres"
)

// fakeRow imitates pgx.Row returned by the pool's QueryRow.
type fakeRow struct {
	scan func(dest ...any) error
}

func (r fakeRow) Scan(dest ...any) error { return r.scan(dest...) }

// fakeQuerier imitates the subset of the pool used by the schema check.
type fakeQuerier struct {
	queries []string
	row     pgx.Row
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.queries = append(q.queries, sql)

	return q.row
}

func TestCheckSchemaVersionAcceptsSupportedVersion(t *testing.T) {
	querier := &fakeQuerier{row: fakeRow{scan: func(dest ...any) error {
		version := dest[0].(*int64)
		dirty := dest[1].(*bool)
		*version = postgres.ExpectedSchemaVersion
		*dirty = false

		return nil
	}}}

	err := postgres.CheckSchemaVersion(t.Context(), querier)

	assert.NoError(t, err)
	require.Len(t, querier.queries, 1)
	assert.Contains(t, querier.queries[0], "schema_migrations")
}

func TestCheckSchemaVersionRejectsBrokenStates(t *testing.T) {
	tests := []struct {
		name      string
		version   int64
		dirty     bool
		wantError error
	}{
		{
			name:      "older schema",
			version:   postgres.ExpectedSchemaVersion - 1,
			wantError: postgres.ErrSchemaNotSupported,
		},
		{
			name:      "newer schema",
			version:   postgres.ExpectedSchemaVersion + 1,
			wantError: postgres.ErrSchemaNotSupported,
		},
		{
			name:      "dirty migration state",
			version:   postgres.ExpectedSchemaVersion,
			dirty:     true,
			wantError: postgres.ErrSchemaDirty,
		},
		{
			name:      "zero version stored",
			version:   0,
			wantError: postgres.ErrSchemaNotSupported,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			querier := &fakeQuerier{row: fakeRow{scan: func(dest ...any) error {
				version := dest[0].(*int64)
				dirty := dest[1].(*bool)
				*version = test.version
				*dirty = test.dirty

				return nil
			}}}

			err := postgres.CheckSchemaVersion(t.Context(), querier)

			assert.ErrorIs(t, err, test.wantError)
			assert.ErrorContains(t, err, "make migrate")
		})
	}
}

func TestCheckSchemaVersionRejectsMissingVersionTable(t *testing.T) {
	querier := &fakeQuerier{row: fakeRow{scan: func(_ ...any) error {
		return &pgconn.PgError{Code: "42P01", Message: "relation \"schema_migrations\" does not exist"}
	}}}

	err := postgres.CheckSchemaVersion(t.Context(), querier)

	assert.ErrorIs(t, err, postgres.ErrSchemaNotMigrated)
}

func TestCheckSchemaVersionTreatsMissingRowAsNotMigrated(t *testing.T) {
	querier := &fakeQuerier{row: fakeRow{scan: func(_ ...any) error { return pgx.ErrNoRows }}}

	err := postgres.CheckSchemaVersion(t.Context(), querier)

	assert.ErrorIs(t, err, postgres.ErrSchemaNotMigrated)
}

var errSyntheticQueryFailure = errors.New("synthetic query failure")

func TestCheckSchemaVersionKeepsUnexpectedErrors(t *testing.T) {
	querier := &fakeQuerier{row: fakeRow{scan: func(_ ...any) error { return errSyntheticQueryFailure }}}

	err := postgres.CheckSchemaVersion(t.Context(), querier)

	require.Error(t, err)
	assert.ErrorIs(t, err, errSyntheticQueryFailure)
	assert.ErrorContains(t, err, "read schema version")
}
