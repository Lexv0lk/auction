// Package category owns the categories reference data: creation, listing,
// renaming and deletion over the shared PostgreSQL pool. The displayed name is
// stored as typed (trimmed); uniqueness is case-insensitive through the
// categories_name_lower_idx index the database enforces.
package category

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxNameLength matches the categories_name_length CHECK of the migration;
// the forms and the service enforce the same limit.
const MaxNameLength = 120

var (
	// ErrNotFound means the category does not exist (or its ID is malformed);
	// handlers render it as 404.
	ErrNotFound = errors.New("category is missing")
	// ErrNameEmpty means the submitted name has no characters after trimming.
	ErrNameEmpty = errors.New("category name is empty")
	// ErrNameTooLong means the name exceeds MaxNameLength characters; the
	// same limit is enforced by the database CHECK constraint.
	ErrNameTooLong = errors.New("category name is too long")
	// ErrExists means another category already carries the same normalized
	// (case-insensitive) name.
	ErrExists = errors.New("category name is already used")
	// ErrInUse means at least one lot references the category; the reference
	// table never loses its catalog entry.
	ErrInUse = errors.New("category is used by lots")
)

// NormalizeName applies the normalization shared with the database index:
// surrounding spaces removed, the display casing preserved.
func NormalizeName(name string) string {
	return strings.TrimSpace(name)
}

// ValidateName normalizes the name and checks it against the length contract
// shared with the database CHECK constraints; the limit counts characters
// (char_length), not bytes.
func ValidateName(name string) error {
	name = NormalizeName(name)
	switch {
	case name == "":
		return ErrNameEmpty
	case utf8.RuneCountInString(name) > MaxNameLength:
		return ErrNameTooLong
	default:
		return nil
	}
}

// Category is one catalog entry.
type Category struct {
	ID   int64
	Name string
}
