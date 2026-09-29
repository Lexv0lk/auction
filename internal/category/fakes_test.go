package category

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// fakeRow scans into the requested destinations from a fixed record.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, target := range dest {
		switch target := target.(type) {
		case *int64:
			*target = r.values[i].(int64)
		case *string:
			*target = r.values[i].(string)
		case *bool:
			*target = r.values[i].(bool)
		default:
			return errFakeRowDestination
		}
	}

	return nil
}

// fakeRows iterates over fixed records; only the members the service uses are
// implemented, the embedded nil interface guards the rest.
type fakeRows struct {
	pgx.Rows
	records [][]any
	cursor  int
	err     error
}

func newFakeRows(records ...[]any) *fakeRows {
	return &fakeRows{records: records}
}

func (r *fakeRows) Next() bool {
	r.cursor++

	return r.cursor <= len(r.records)
}

func (r *fakeRows) Scan(dest ...any) error {
	return fakeRow{values: r.records[r.cursor-1]}.Scan(dest...)
}

func (r *fakeRows) Err() error {
	return r.err
}

func (r *fakeRows) Close() {}

type queryCall struct {
	sql  string
	args []any
}

type fakePool struct {
	row       pgx.Row // the usage pre-check and every non-delete query
	deleteRow pgx.Row // the DELETE ... RETURNING statement
	rows      pgx.Rows
	rowsErr   error
	rowCalls  []queryCall
	queryUsed bool
}

func (p *fakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	p.rowCalls = append(p.rowCalls, queryCall{sql: sql, args: args})
	if strings.Contains(sql, "DELETE FROM categories") {
		return p.deleteRow
	}

	return p.row
}

func (p *fakePool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	p.queryUsed = true
	if p.rowsErr != nil {
		return nil, p.rowsErr
	}

	return p.rows, nil
}
