package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
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
		case **int64:
			// A nil record value is a SQL NULL: the pointer stays nil.
			if value, ok := r.values[i].(int64); ok {
				value := value
				*target = &value
			} else {
				*target = nil
			}
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

type queryCall struct {
	sql  string
	args []any
}

// fakeTx serves one transaction of the worker. Every statement is routed to
// its fixed answer by a SQL substring, the statements and their arguments are
// recorded in order, and Commit/Rollback set their flags.
type fakeTx struct {
	pgx.Tx // embedded nil interface guards the unused members
	mu     sync.Mutex

	responses []fakeTxResponse
	commitErr error

	queries    []queryCall
	committed  bool
	rolledBack bool
}

type fakeTxResponse struct {
	contains string
	row      fakeRow
}

func (tx *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.queries = append(tx.queries, queryCall{sql: sql, args: args})
	for _, response := range tx.responses {
		if strings.Contains(sql, response.contains) {
			return response.row
		}
	}

	return fakeRow{err: errFakeRowDestination}
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.committed = true

	return tx.commitErr
}

func (tx *fakeTx) Rollback(context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	tx.rolledBack = true

	return nil
}

func (tx *fakeTx) recordedQueries() []queryCall {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	return append([]queryCall(nil), tx.queries...)
}

// fakePool hands every BeginTx call a transaction built by its factory. The
// handed-out transactions and the requested isolation levels are kept for the
// assertions; a nil factory produces the standard no-work transaction.
type fakePool struct {
	mu        sync.Mutex
	beginErr  error
	factory   func() *fakeTx
	txOptions []pgx.TxOptions
	txs       []*fakeTx
}

func (p *fakePool) BeginTx(_ context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.txOptions = append(p.txOptions, options)
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	newTx := &fakeTx{}
	if p.factory != nil {
		newTx = p.factory()
	}
	p.txs = append(p.txs, newTx)

	return newTx, nil
}

func (p *fakePool) beginCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.txOptions)
}

func (p *fakePool) handedOut() []*fakeTx {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]*fakeTx(nil), p.txs...)
}
