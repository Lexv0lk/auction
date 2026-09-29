// Package lot owns the auction lots: draft CRUD for administrators and the
// publication operation that opens the bidding with immutable conditions. A
// lot is always created as a draft; status, result fields and the deadline
// relative to the database clock are decided by the application operations,
// never by form input.
package lot

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Lot statuses match the lots_status_check values of the migration.
const (
	StatusDraft    = "draft"
	StatusActive   = "active"
	StatusFinished = "finished"
)

// Length limits match the title and description CHECK constraints of the
// migration; the forms and the service enforce the same limits. Both count
// characters (char_length), not bytes.
const (
	MaxTitleLength       = 200
	MaxDescriptionLength = 5000
)

var (
	// ErrNotFound means the lot does not exist (or its ID is malformed);
	// handlers render it as 404.
	ErrNotFound = errors.New("lot is missing")
	// ErrTitleEmpty means the submitted title has no characters after trimming.
	ErrTitleEmpty = errors.New("lot title is empty")
	// ErrTitleTooLong means the title exceeds MaxTitleLength characters; the
	// same limit is enforced by the database CHECK constraint.
	ErrTitleTooLong = errors.New("lot title is too long")
	// ErrDescriptionEmpty means the submitted description has no characters
	// after trimming.
	ErrDescriptionEmpty = errors.New("lot description is empty")
	// ErrDescriptionTooLong means the description exceeds
	// MaxDescriptionLength characters.
	ErrDescriptionTooLong = errors.New("lot description is too long")
	// ErrCategoryRequired means the category is missing or malformed; the
	// lot always references an existing category.
	ErrCategoryRequired = errors.New("lot category is required")
	// ErrCategoryMissing means the referenced category does not exist; the
	// foreign key on lots.category_id is the final guard.
	ErrCategoryMissing = errors.New("lot category does not exist")
	// ErrStartPriceNonPositive means the start price is zero or negative;
	// the same rule is enforced by the database CHECK constraint.
	ErrStartPriceNonPositive = errors.New("lot start price must be positive")
	// ErrEndsAtMissing means the deadline is absent.
	ErrEndsAtMissing = errors.New("lot deadline is required")
	// ErrNotDraft means the lot has left the draft state: its conditions are
	// immutable, so editing, deletion and republication are refused (409).
	ErrNotDraft = errors.New("lot is not a draft")
	// ErrNotPublishable means the stored draft fails the completeness check;
	// drafts created through the application are always complete, so this is
	// a defensive stop for data changed outside the application.
	ErrNotPublishable = errors.New("lot data is incomplete, publication is refused")
	// ErrDeadlinePast means the deadline is not in the future by the
	// database clock; the lot stays a draft and the deadline can be edited.
	ErrDeadlinePast = errors.New("lot deadline is not in the future")
)

// Lot is one auction lot as stored; CategoryName is the display name of the
// referenced category, filled by the reads.
type Lot struct {
	ID           int64
	Title        string
	Description  string
	CategoryID   int64
	CategoryName string
	StartPrice   int64
	Status       string
	EndsAt       time.Time
}

// Input carries the administrator-editable fields of a draft. Service fields
// (status, winning_bid_id, finished_at) are deliberately absent: no operation
// of this package accepts them from the outside.
type Input struct {
	Title       string
	Description string
	CategoryID  int64
	StartPrice  int64
	EndsAt      time.Time
}

// NormalizeInput applies the normalization shared with the database CHECKs:
// surrounding spaces removed from the strings, the rest preserved.
func NormalizeInput(input Input) Input {
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)

	return input
}

// ValidateInput normalizes the input and reports the first broken field in
// the fixed order title, description, category, start price, deadline. A past
// deadline is legal for drafts: only the publication operation compares the
// deadline with the database clock.
func ValidateInput(input Input) error {
	input = NormalizeInput(input)
	switch {
	case input.Title == "":
		return ErrTitleEmpty
	case utf8.RuneCountInString(input.Title) > MaxTitleLength:
		return ErrTitleTooLong
	case input.Description == "":
		return ErrDescriptionEmpty
	case utf8.RuneCountInString(input.Description) > MaxDescriptionLength:
		return ErrDescriptionTooLong
	case input.CategoryID <= 0:
		return ErrCategoryRequired
	case input.StartPrice <= 0:
		return ErrStartPriceNonPositive
	case input.EndsAt.IsZero():
		return ErrEndsAtMissing
	default:
		return nil
	}
}

// inputFromLot rebuilds the editable fields of a stored lot, used by the
// publication completeness check.
func inputFromLot(l Lot) Input {
	return Input{
		Title:       l.Title,
		Description: l.Description,
		CategoryID:  l.CategoryID,
		StartPrice:  l.StartPrice,
		EndsAt:      l.EndsAt,
	}
}
