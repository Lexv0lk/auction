package seed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/postgres"
)

// fakePool fakes the two pool methods the seed uses; the Scan of the version
// row is served from the struct fields.
type fakePool struct {
	version     int64
	dirty       bool
	scanErr     error
	beginCalled bool
}

func (f *fakePool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return f
}

func (f *fakePool) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	*(dest[0].(*int64)) = f.version
	*(dest[1].(*bool)) = f.dirty

	return nil
}

func (f *fakePool) Begin(_ context.Context) (pgx.Tx, error) {
	f.beginCalled = true

	return nil, errors.New("fake pool does not open transactions")
}

func validOptions() Options {
	return Options{AdminPassword: "admin-password", ParticipantPassword: "participant-password"}
}

func TestRunRejectsWrongSchemaVersionBeforeTouchingData(t *testing.T) {
	cases := []struct {
		name    string
		pool    *fakePool
		wantErr error
	}{
		{"not migrated", &fakePool{scanErr: pgx.ErrNoRows}, postgres.ErrSchemaNotMigrated},
		{"dirty", &fakePool{version: 4, dirty: true}, postgres.ErrSchemaDirty},
		{"older version", &fakePool{version: 3}, postgres.ErrSchemaNotSupported},
		{"newer version", &fakePool{version: 5}, postgres.ErrSchemaNotSupported},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			summary, err := Run(context.Background(), testCase.pool, validOptions())

			require.ErrorIs(t, err, testCase.wantErr)
			assert.Equal(t, Summary{}, summary)
			assert.False(t, testCase.pool.beginCalled, "no transaction must start for a rejected schema")
		})
	}
}

func TestRunRejectsMissingPasswords(t *testing.T) {
	cases := []struct {
		name    string
		options Options
		missing string
	}{
		{"no admin password", Options{ParticipantPassword: "p"}, "SEED_ADMIN_PASSWORD"},
		{"no participant password", Options{AdminPassword: "a"}, "SEED_PARTICIPANT_PASSWORD"},
		{"no passwords at all", Options{}, "SEED_ADMIN_PASSWORD"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pool := &fakePool{version: 4}

			_, err := Run(context.Background(), pool, testCase.options)

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.missing)
			assert.False(t, pool.beginCalled, "no transaction must start without passwords")
		})
	}
}

func TestRunRejectsPasswordsOverBcryptLimit(t *testing.T) {
	pool := &fakePool{version: 4}
	options := Options{
		AdminPassword:       strings.Repeat("a", 73),
		ParticipantPassword: "participant-password",
	}

	_, err := Run(context.Background(), pool, options)

	require.ErrorIs(t, err, password.ErrPasswordTooLong)
	assert.False(t, pool.beginCalled)
}

func TestRunErrorsNeverContainPasswords(t *testing.T) {
	pool := &fakePool{version: 3}

	_, err := Run(context.Background(), pool, Options{
		AdminPassword:       "secret admin value",
		ParticipantPassword: "secret participant value",
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret admin value")
	assert.NotContains(t, err.Error(), "secret participant value")
}

func TestSummaryStringReportsCreatedAndSkippedCounts(t *testing.T) {
	summary := Summary{
		AdminCreated:        1,
		AdminSkipped:        1,
		ParticipantsCreated: 3,
		ParticipantsSkipped: 3,
		CategoriesCreated:   4,
		CategoriesSkipped:   4,
		LotsCreated:         4,
		LotsSkipped:         4,
	}

	assert.Equal(t,
		"seed complete: admin created=1 skipped=1, participants created=3 skipped=3, "+
			"categories created=4 skipped=4, lots created=4 skipped=4",
		summary.String())
}
