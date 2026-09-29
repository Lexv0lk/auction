package category

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL error codes the service translates into domain outcomes; the
// constraint names pin the mapping to exactly the reference-data contracts.
const (
	pgErrUniqueViolation     = "23505"
	pgErrForeignKeyViolation = "23503"
)

const (
	uniqueNameConstraint = "categories_name_lower_idx"
	categoryFKConstraint = "lots_category_id_fk"

	listCategoriesSQL = "SELECT id, name FROM categories ORDER BY lower(name), id"
	insertCategorySQL = "INSERT INTO categories (name) VALUES ($1) RETURNING id, name"
	selectCategorySQL = "SELECT id, name FROM categories WHERE id = $1"
	renameCategorySQL = "UPDATE categories SET name = $2 WHERE id = $1 RETURNING id, name"
	// The usage pre-check produces a friendly message, but the foreign key on
	// lots.category_id stays the final guard against a lot created
	// concurrently.
	selectCategoryUsedSQL = "SELECT EXISTS (SELECT 1 FROM lots WHERE category_id = $1)"
	deleteCategorySQL     = "DELETE FROM categories WHERE id = $1 RETURNING id"
)

// Pool is the subset of the connection pool the category service needs.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Service implements the reference-data operations on top of the shared
// connection pool. Every statement is parameterized; SQL text never carries
// user input.
type Service struct {
	pool Pool
}

// NewService builds the category service on top of the shared connection pool.
func NewService(pool Pool) *Service {
	return &Service{pool: pool}
}

// List returns every category ordered by its normalized name.
func (s *Service) List(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx, listCategoriesSQL)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	defer rows.Close()

	categories := []Category{}
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		categories = append(categories, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read categories: %w", err)
	}

	return categories, nil
}

// Create validates the normalized name and inserts the category. A duplicate
// (case-insensitive) name is the ErrExists field error, not a technical
// failure: the unique index is the shared source of uniqueness.
func (s *Service) Create(ctx context.Context, name string) (Category, error) {
	name = NormalizeName(name)
	if err := ValidateName(name); err != nil {
		return Category{}, err
	}

	var c Category
	err := s.pool.QueryRow(ctx, insertCategorySQL, name).Scan(&c.ID, &c.Name)
	if err != nil {
		if mapped := mapConstraintError(err); mapped != nil {
			return Category{}, mapped
		}

		return Category{}, fmt.Errorf("create category: %w", err)
	}

	return c, nil
}

// Get loads one category; a missing ID is ErrNotFound, which handlers render
// as 404.
func (s *Service) Get(ctx context.Context, id int64) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx, selectCategorySQL, id).Scan(&c.ID, &c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, fmt.Errorf("load category: %w", err)
	}

	return c, nil
}

// Rename validates the new name and updates the category in place; the ID and
// the creation history never change.
func (s *Service) Rename(ctx context.Context, id int64, name string) (Category, error) {
	name = NormalizeName(name)
	if err := ValidateName(name); err != nil {
		return Category{}, err
	}

	var c Category
	err := s.pool.QueryRow(ctx, renameCategorySQL, id, name).Scan(&c.ID, &c.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		if mapped := mapConstraintError(err); mapped != nil {
			return Category{}, mapped
		}

		return Category{}, fmt.Errorf("rename category: %w", err)
	}

	return c, nil
}

// Delete removes an unused category. Lots referencing the category block the
// deletion: the pre-check answers with a friendly ErrInUse before touching
// the row, and the foreign key converts a concurrent lot creation into the
// same error, so a reference can never dangle. A category that is already
// gone is ErrNotFound.
func (s *Service) Delete(ctx context.Context, id int64) error {
	var used bool
	err := s.pool.QueryRow(ctx, selectCategoryUsedSQL, id).Scan(&used)
	if err != nil {
		return fmt.Errorf("check category usage: %w", err)
	}
	if used {
		return ErrInUse
	}

	var deleted int64
	err = s.pool.QueryRow(ctx, deleteCategorySQL, id).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		if mapped := mapConstraintError(err); mapped != nil {
			return mapped
		}

		return fmt.Errorf("delete category: %w", err)
	}

	return nil
}

// mapConstraintError returns the domain error for the reference-data
// constraint violations and nil for every other error.
func mapConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}

	switch {
	case pgErr.Code == pgErrUniqueViolation && pgErr.ConstraintName == uniqueNameConstraint:
		return ErrExists
	case pgErr.Code == pgErrForeignKeyViolation && pgErr.ConstraintName == categoryFKConstraint:
		return ErrInUse
	default:
		return nil
	}
}
