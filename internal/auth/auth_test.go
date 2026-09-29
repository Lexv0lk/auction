package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/password"
)

const testPasswordHash = "$2a$10$ei1DGGYqMeIUA9UHkYDSNeYuOWKfKtqufKj0lskxMvlbd1a5g1an."

var (
	errFakeRowDestination   = errors.New("fakeRow: unsupported destination")
	errFakeMustNotQuery     = errors.New("fake pool: this query must not run")
	errFakeConnectionBroken = errors.New("connection refused")
)

func TestNormalizeLogin(t *testing.T) {
	assert.Equal(t, "demo-admin", NormalizeLogin("  Demo-Admin "))
	assert.Equal(t, "", NormalizeLogin("   "))
}

func TestUserContext(t *testing.T) {
	ctx := context.Background()
	_, ok := UserFromContext(ctx)
	assert.False(t, ok, "context without a user must not report one")

	user := User{ID: 3, Login: "demo-admin", Role: RoleAdmin}
	loaded, ok := UserFromContext(WithUser(ctx, user))
	assert.True(t, ok)
	assert.Equal(t, user, loaded)
}

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
		source := r.values[i]
		switch target := target.(type) {
		case *int64:
			*target = source.(int64)
		case *string:
			*target = source.(string)
		default:
			return errFakeRowDestination
		}
	}

	return nil
}

type execCall struct {
	sql  string
	args []any
}

type fakePool struct {
	userRow    pgx.Row
	userCalls  int
	sessionRow pgx.Row
	execCalls  []execCall
	execErr    error
}

func (p *fakePool) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "FROM users") {
		p.userCalls++

		return p.userRow
	}

	return p.sessionRow
}

func (p *fakePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.execCalls = append(p.execCalls, execCall{sql: sql, args: args})
	if p.execErr != nil {
		return pgconn.CommandTag{}, p.execErr
	}

	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}

func TestLoginUnknownLoginAndWrongPasswordBehaveTheSame(t *testing.T) {
	// Unknown login: the query finds no row, the dummy hash still burns the
	// same bcrypt time and no session is created.
	unknownPool := &fakePool{userRow: fakeRow{err: pgx.ErrNoRows}}
	service := NewService(unknownPool)

	token, user, err := service.Login(context.Background(), "ghost", "secret", 0)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	assert.Empty(t, token)
	assert.Equal(t, User{}, user)
	assert.Empty(t, unknownPool.execCalls, "no session must be stored for an unknown login")

	// Wrong password: the account exists but verification fails, same error.
	wrongPool := &fakePool{userRow: fakeRow{values: []any{int64(7), RoleAdmin, testPasswordHash}}}
	service = NewService(wrongPool)

	token, user, err = service.Login(context.Background(), "demo-admin", "not-the-password", 0)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
	assert.Empty(t, token)
	assert.Equal(t, User{}, user)
	assert.Empty(t, wrongPool.execCalls, "no session must be stored for a wrong password")
}

func TestLoginRejectsEmptyAndOversizedFieldsWithoutDatabase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		login    string
		password string
	}{
		{"empty login", "   ", "secret"},
		{"empty password", "demo-admin", ""},
		{"oversized login", strings.Repeat("a", 65), "secret"},
		{"oversized password", "demo-admin", strings.Repeat("a", 73)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &fakePool{userRow: fakeRow{err: errFakeMustNotQuery}}
			service := NewService(pool)

			token, user, err := service.Login(context.Background(), tc.login, tc.password, 0)
			assert.ErrorIs(t, err, ErrInvalidCredentials)
			assert.Empty(t, token)
			assert.Equal(t, User{}, user)
			assert.Zero(t, pool.userCalls, "invalid fields must be rejected before the database")
		})
	}
}

func TestLoginSuccessStoresHashedSession(t *testing.T) {
	storedHash, err := password.Hash("secret")
	require.NoError(t, err)
	pool := &fakePool{userRow: fakeRow{values: []any{int64(7), RoleAdmin, storedHash}}}
	service := NewService(pool)

	token, user, err := service.Login(context.Background(), " Demo-Admin ", "secret", 3600)
	require.NoError(t, err)
	assert.Equal(t, User{ID: 7, Login: "demo-admin", Role: RoleAdmin}, user)
	assert.Len(t, token, 64, "token is 32 random bytes hex-encoded")
	_, err = hex.DecodeString(token)
	assert.NoError(t, err, "token must be hexadecimal")

	require.Len(t, pool.execCalls, 1, "exactly one session insert")
	call := pool.execCalls[0]
	assert.Contains(t, call.sql, "INSERT INTO sessions")
	require.Len(t, call.args, 3)
	storedHash, ok := call.args[0].(string)
	require.True(t, ok)
	assert.Equal(t, sha256Hex(token), storedHash, "the database stores the SHA-256 hash of the token")
	assert.NotEqual(t, token, storedHash, "the raw token must never reach the database")
	assert.Equal(t, int64(7), call.args[1])
}

func TestLoginCorruptStoredHashCountsAsInvalidCredentials(t *testing.T) {
	pool := &fakePool{userRow: fakeRow{values: []any{int64(7), RoleAdmin, "not-a-bcrypt-hash"}}}
	service := NewService(pool)

	_, _, err := service.Login(context.Background(), "demo-admin", "secret", 3600)
	assert.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestLoginMapsDatabaseErrorsApartFromCredentials(t *testing.T) {
	pool := &fakePool{userRow: fakeRow{err: errFakeConnectionBroken}}
	service := NewService(pool)

	_, _, err := service.Login(context.Background(), "demo-admin", "secret", 3600)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidCredentials, "a database failure must not look like a wrong password")
}

func TestSessionUserLoadsUserAndRejectsMissingSessions(t *testing.T) {
	pool := &fakePool{sessionRow: fakeRow{values: []any{int64(3), "demo-participant-1", RoleParticipant}}}
	service := NewService(pool)

	user, err := service.User(context.Background(), strings.Repeat("a", 64))
	require.NoError(t, err)
	assert.Equal(t, User{ID: 3, Login: "demo-participant-1", Role: RoleParticipant}, user)

	pool = &fakePool{sessionRow: fakeRow{err: pgx.ErrNoRows}}
	service = NewService(pool)
	_, err = service.User(context.Background(), strings.Repeat("b", 64))
	assert.ErrorIs(t, err, ErrNoSession, "unknown, expired and substituted tokens look the same")

	pool = &fakePool{sessionRow: fakeRow{err: errFakeConnectionBroken}}
	service = NewService(pool)
	_, err = service.User(context.Background(), strings.Repeat("c", 64))
	assert.NotErrorIs(t, err, ErrNoSession, "a database failure must not close the session")
}

func TestLogoutDeletesSession(t *testing.T) {
	pool := &fakePool{}
	service := NewService(pool)

	err := service.Logout(context.Background(), strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Len(t, pool.execCalls, 1)
	assert.Contains(t, pool.execCalls[0].sql, "DELETE FROM sessions")
	require.Len(t, pool.execCalls[0].args, 1)
	assert.Equal(t, sha256Hex(strings.Repeat("a", 64)), pool.execCalls[0].args[0], "logout deletes by the stored hash")

	pool = &fakePool{execErr: errFakeConnectionBroken}
	service = NewService(pool)
	err = service.Logout(context.Background(), strings.Repeat("b", 64))
	assert.Error(t, err)
}

func TestPasswordPackageCompatibility(t *testing.T) {
	// The seed stores hashes created by internal/password; the login code
	// verifies them with the same library (step 04 compatibility contract).
	hash, err := password.Hash("secret")
	require.NoError(t, err)
	assert.NoError(t, password.Verify(hash, "secret"))
	assert.Error(t, password.Verify(hash, "wrong"))
}
