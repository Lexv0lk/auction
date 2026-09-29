package lot

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testInput() Input {
	return Input{
		Title:       "Серебряный рубль 1726 года",
		Description: "Монета в хорошем состоянии",
		CategoryID:  3,
		StartPrice:  5000,
		EndsAt:      time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
	}
}

func TestValidateInputAcceptsCompleteInput(t *testing.T) {
	assert.NoError(t, ValidateInput(testInput()))
}

func TestValidateInputTrimsStrings(t *testing.T) {
	endsAt := testInput().EndsAt
	normalized := NormalizeInput(Input{
		Title:       "  Рубль 1726 года\t",
		Description: " \nОписание с пробелами  ",
		CategoryID:  3,
		StartPrice:  5000,
		EndsAt:      endsAt,
	})

	assert.Equal(t, "Рубль 1726 года", normalized.Title)
	assert.Equal(t, "Описание с пробелами", normalized.Description)
	// Numbers and the deadline are untouched by normalization.
	assert.Equal(t, int64(3), normalized.CategoryID)
	assert.Equal(t, int64(5000), normalized.StartPrice)
	assert.True(t, endsAt.Equal(normalized.EndsAt))
}

func TestValidateInputFieldOrder(t *testing.T) {
	// The first broken field in the fixed order wins, so the form explains
	// one problem at a time in a predictable place.
	empty := Input{}
	assert.ErrorIs(t, ValidateInput(empty), ErrTitleEmpty)

	titleOnly := Input{Title: "Рубль"}
	assert.ErrorIs(t, ValidateInput(titleOnly), ErrDescriptionEmpty)

	noCategory := Input{Title: "Рубль", Description: "Описание"}
	assert.ErrorIs(t, ValidateInput(noCategory), ErrCategoryRequired)

	noPrice := Input{Title: "Рубль", Description: "Описание", CategoryID: 3}
	assert.ErrorIs(t, ValidateInput(noPrice), ErrStartPriceNonPositive)

	noDeadline := Input{Title: "Рубль", Description: "Описание", CategoryID: 3, StartPrice: 5000}
	assert.ErrorIs(t, ValidateInput(noDeadline), ErrEndsAtMissing)
}

func TestValidateTitleLength(t *testing.T) {
	tooLong := testInput()
	tooLong.Title = strings.Repeat("р", MaxTitleLength+1)
	assert.ErrorIs(t, ValidateInput(tooLong), ErrTitleTooLong)

	// The limit counts characters (char_length in the database CHECK), not
	// bytes: a full-length Cyrillic title is twice as many bytes and is legal.
	atLimit := testInput()
	atLimit.Title = strings.Repeat("р", MaxTitleLength)
	assert.NoError(t, ValidateInput(atLimit))

	blank := testInput()
	blank.Title = "   "
	assert.ErrorIs(t, ValidateInput(blank), ErrTitleEmpty, "spaces are not a title")
}

func TestValidateDescriptionLength(t *testing.T) {
	tooLong := testInput()
	tooLong.Description = strings.Repeat("о", MaxDescriptionLength+1)
	assert.ErrorIs(t, ValidateInput(tooLong), ErrDescriptionTooLong)

	atLimit := testInput()
	atLimit.Description = strings.Repeat("о", MaxDescriptionLength)
	assert.NoError(t, ValidateInput(atLimit))

	blank := testInput()
	blank.Description = " \t "
	assert.ErrorIs(t, ValidateInput(blank), ErrDescriptionEmpty)
}

func TestValidateNumbers(t *testing.T) {
	zeroPrice := testInput()
	zeroPrice.StartPrice = 0
	assert.ErrorIs(t, ValidateInput(zeroPrice), ErrStartPriceNonPositive)

	negativePrice := testInput()
	negativePrice.StartPrice = -1
	assert.ErrorIs(t, ValidateInput(negativePrice), ErrStartPriceNonPositive)

	maxPrice := testInput()
	maxPrice.StartPrice = 9223372036854775807
	assert.NoError(t, ValidateInput(maxPrice), "int64 max is a legal start price")

	zeroCategory := testInput()
	zeroCategory.CategoryID = 0
	assert.ErrorIs(t, ValidateInput(zeroCategory), ErrCategoryRequired)

	negativeCategory := testInput()
	negativeCategory.CategoryID = -5
	assert.ErrorIs(t, ValidateInput(negativeCategory), ErrCategoryRequired)
}

func TestValidateDeadline(t *testing.T) {
	noDeadline := testInput()
	noDeadline.EndsAt = time.Time{}
	assert.ErrorIs(t, ValidateInput(noDeadline), ErrEndsAtMissing)

	// A past deadline is legal for drafts: only the publication operation
	// compares the deadline with the database clock.
	past := testInput()
	past.EndsAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.NoError(t, ValidateInput(past))
}

func TestStatusConstants(t *testing.T) {
	// The labels must match the lots_status_check values of the migration.
	assert.Equal(t, "draft", StatusDraft)
	assert.Equal(t, "active", StatusActive)
	assert.Equal(t, "finished", StatusFinished)
}
