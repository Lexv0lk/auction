package lot

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errFakeRowDestination = errors.New("fakeRow: unsupported destination")

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
		case *time.Time:
			*target = r.values[i].(time.Time)
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

func (r *fakeRows) Err() error { return nil }

func (r *fakeRows) Close() {}

type queryCall struct {
	sql  string
	args []any
}

// fakeTx records the statements of one transaction. lockRow answers the
// SELECT ... FOR UPDATE probe, changeRow answers the RETURNING statements of
// the state changes.
type fakeTx struct {
	pgx.Tx    // embedded nil interface guards the unused members
	lockRow   fakeRow
	changeRow fakeRow
	commitErr error

	queries    []queryCall
	committed  bool
	rolledBack bool
}

func (tx *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.queries = append(tx.queries, queryCall{sql: sql, args: args})
	if strings.Contains(sql, "UPDATE lots SET") ||
		strings.Contains(sql, "DELETE FROM lots") ||
		strings.Contains(sql, "status = 'active'") {
		return tx.changeRow
	}

	return tx.lockRow
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.committed = true

	return tx.commitErr
}

func (tx *fakeTx) Rollback(context.Context) error {
	tx.rolledBack = true

	return nil
}

// fakePool serves the non-transactional statements and hands out the same
// fakeTx to every Begin call.
type fakePool struct {
	row      fakeRow // Create INSERT ... RETURNING and Get
	rows     *fakeRows
	beginErr error
	tx       *fakeTx

	queries []queryCall
}

func (p *fakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	p.queries = append(p.queries, queryCall{sql: sql, args: args})

	return p.row
}

func (p *fakePool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return p.rows, nil
}

func (p *fakePool) Begin(context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	if p.tx == nil {
		p.tx = &fakeTx{}
	}

	return p.tx, nil
}
