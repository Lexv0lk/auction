package lot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL error codes the service translates into domain outcomes; the
// constraint name pins the mapping to exactly the category reference of the
// lot (the same FK the category deletion refuses to break).
const (
	pgErrForeignKeyViolation = "23503"

	categoryFKConstraint = "lots_category_id_fk"
	bidsUserFKConstraint = "bids_user_id_fk"
)

// The reads join the category name for display. The state changes follow one
// fixed order — the lot row first: it is locked with FOR UPDATE, the draft
// status is verified under the lock, and only then the row is changed.
const (
	listLotsSQL = "SELECT l.id, l.title, l.description, l.category_id, c.name, l.start_price, l.status, l.ends_at" +
		" FROM lots l JOIN categories c ON c.id = l.category_id" +
		" ORDER BY l.id DESC"
	selectLotSQL = "SELECT l.id, l.title, l.description, l.category_id, c.name, l.start_price, l.status, l.ends_at" +
		" FROM lots l JOIN categories c ON c.id = l.category_id" +
		" WHERE l.id = $1"
	// The draft status is always checked under the row lock, so a publication
	// racing the update or delete resolves to one consistent version.
	lockLotForUpdateSQL  = "SELECT id, status FROM lots WHERE id = $1 FOR UPDATE"
	lockLotForPublishSQL = "SELECT id, title, description, category_id, start_price, status, ends_at, now()" +
		" FROM lots WHERE id = $1 FOR UPDATE"
	// A new lot is always a draft: the INSERT never takes a status from the
	// outside.
	insertLotSQL = "INSERT INTO lots (title, description, category_id, start_price, status, ends_at)" +
		" VALUES ($1, $2, $3, $4, 'draft', $5) RETURNING id, status"
	updateLotSQL = "UPDATE lots SET title = $2, description = $3, category_id = $4, start_price = $5, ends_at = $6" +
		" WHERE id = $1 RETURNING id"
	deleteLotSQL  = "DELETE FROM lots WHERE id = $1 RETURNING id"
	publishLotSQL = "UPDATE lots SET status = 'active' WHERE id = $1 RETURNING id"
)

// Pool is the subset of the connection pool the lot service needs. Reads run
// outside transactions; every state change wraps its lock-and-check sequence
// into one transaction (pgx.Tx). BeginTx lets the bid operation pin its
// isolation level instead of trusting the database default.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// Service implements the draft CRUD and publication over the shared
// PostgreSQL pool. Every statement is parameterized; SQL text never carries
// user input.
type Service struct {
	pool Pool
}

// NewService builds the lot service on top of the shared connection pool.
func NewService(pool Pool) *Service {
	return &Service{pool: pool}
}

// List returns every lot of every status, newest first: the administrative
// screen shows drafts, running and finished auctions together.
func (s *Service) List(ctx context.Context) ([]Lot, error) {
	rows, err := s.pool.Query(ctx, listLotsSQL)
	if err != nil {
		return nil, fmt.Errorf("list lots: %w", err)
	}
	defer rows.Close()

	lots := []Lot{}
	for rows.Next() {
		var l Lot
		if err := rows.Scan(&l.ID, &l.Title, &l.Description, &l.CategoryID, &l.CategoryName,
			&l.StartPrice, &l.Status, &l.EndsAt); err != nil {
			return nil, fmt.Errorf("scan lot: %w", err)
		}
		lots = append(lots, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lots: %w", err)
	}

	return lots, nil
}

// Get loads one lot with its category name; a missing ID is ErrNotFound.
func (s *Service) Get(ctx context.Context, id int64) (Lot, error) {
	var l Lot
	err := s.pool.QueryRow(ctx, selectLotSQL, id).
		Scan(&l.ID, &l.Title, &l.Description, &l.CategoryID, &l.CategoryName, &l.StartPrice, &l.Status, &l.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lot{}, ErrNotFound
	}
	if err != nil {
		return Lot{}, fmt.Errorf("load lot: %w", err)
	}

	return l, nil
}

// Create validates the input and inserts the lot as a draft. The insert is a
// single statement, so no transaction is needed; the foreign key converts an
// unknown category into ErrCategoryMissing.
func (s *Service) Create(ctx context.Context, input Input) (Lot, error) {
	input = NormalizeInput(input)
	if err := ValidateInput(input); err != nil {
		return Lot{}, err
	}

	var id int64
	var status string
	err := s.pool.QueryRow(ctx, insertLotSQL,
		input.Title, input.Description, input.CategoryID, input.StartPrice, input.EndsAt).
		Scan(&id, &status)
	if err != nil {
		if mapped := mapConstraintError(err); mapped != nil {
			return Lot{}, mapped
		}

		return Lot{}, fmt.Errorf("create lot: %w", err)
	}

	return Lot{
		ID:          id,
		Title:       input.Title,
		Description: input.Description,
		CategoryID:  input.CategoryID,
		StartPrice:  input.StartPrice,
		Status:      status,
		EndsAt:      input.EndsAt,
	}, nil
}

// Update validates the input and rewrites the editable fields of a draft.
// The lot row is locked first and its status checked under the lock: a
// publication racing the update either happens entirely before (the update
// is refused with ErrNotDraft) or entirely after (the publication opens the
// bidding with exactly the edited conditions). The status itself never
// changes here.
func (s *Service) Update(ctx context.Context, id int64, input Input) (Lot, error) {
	input = NormalizeInput(input)
	if err := ValidateInput(input); err != nil {
		return Lot{}, err
	}

	var status string
	if err := s.withLockedLot(ctx, func(tx pgx.Tx) error {
		var err error
		status, err = lockLotStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if status != StatusDraft {
			return ErrNotDraft
		}

		var updated int64
		err = tx.QueryRow(ctx, updateLotSQL,
			id, input.Title, input.Description, input.CategoryID, input.StartPrice, input.EndsAt).
			Scan(&updated)
		if err != nil {
			if mapped := mapConstraintError(err); mapped != nil {
				return mapped
			}

			return fmt.Errorf("update lot: %w", err)
		}

		return nil
	}); err != nil {
		return Lot{}, err
	}

	return Lot{
		ID:          id,
		Title:       input.Title,
		Description: input.Description,
		CategoryID:  input.CategoryID,
		StartPrice:  input.StartPrice,
		Status:      status,
		EndsAt:      input.EndsAt,
	}, nil
}

// Delete removes a draft. Active and finished lots are immutable and are
// refused with ErrNotDraft under the row lock.
func (s *Service) Delete(ctx context.Context, id int64) error {
	return s.withLockedLot(ctx, func(tx pgx.Tx) error {
		status, err := lockLotStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if status != StatusDraft {
			return ErrNotDraft
		}

		var deleted int64
		if err := tx.QueryRow(ctx, deleteLotSQL, id).Scan(&deleted); err != nil {
			return fmt.Errorf("delete lot: %w", err)
		}

		return nil
	})
}

// Publish opens the bidding for a draft: the lot row is locked, the draft
// status and the completeness of the stored fields are verified, the deadline
// is checked against the current database time (now() of the same
// transaction), and only then the status becomes active inside the same
// transaction. Republishing an active or finished lot is ErrNotDraft and can
// never extend the deadline; a past deadline leaves the draft untouched.
func (s *Service) Publish(ctx context.Context, id int64) (Lot, error) {
	var stored Lot
	if err := s.withLockedLot(ctx, func(tx pgx.Tx) error {
		var dbNow time.Time
		err := tx.QueryRow(ctx, lockLotForPublishSQL, id).
			Scan(&stored.ID, &stored.Title, &stored.Description, &stored.CategoryID,
				&stored.StartPrice, &stored.Status, &stored.EndsAt, &dbNow)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock lot for publication: %w", err)
		}
		if stored.Status != StatusDraft {
			return ErrNotDraft
		}
		if err := ValidateInput(inputFromLot(stored)); err != nil {
			return ErrNotPublishable
		}
		if !stored.EndsAt.After(dbNow) {
			return ErrDeadlinePast
		}

		var published int64
		if err := tx.QueryRow(ctx, publishLotSQL, id).Scan(&published); err != nil {
			return fmt.Errorf("publish lot: %w", err)
		}

		return nil
	}); err != nil {
		return Lot{}, err
	}

	stored.Status = StatusActive

	return stored, nil
}

// withLockedLot runs the work inside one transaction that begins and commits
// around it; the rollback in the defer is a no-op after a successful commit.
func (s *Service) withLockedLot(ctx context.Context, work func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := work(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit lot transaction: %w", err)
	}

	return nil
}

func lockLotStatus(ctx context.Context, tx pgx.Tx, id int64) (string, error) {
	var lockedID int64
	var status string
	err := tx.QueryRow(ctx, lockLotForUpdateSQL, id).Scan(&lockedID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock lot: %w", err)
	}

	return status, nil
}

// mapConstraintError returns the domain error for the foreign keys the
// operations translate — the lot's category and the bid's participant — and
// nil for every other error.
func mapConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}

	if pgErr.Code != pgErrForeignKeyViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case categoryFKConstraint:
		return ErrCategoryMissing
	case bidsUserFKConstraint:
		return ErrParticipantMissing
	default:
		return nil
	}
}
