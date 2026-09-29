//go:build integration

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/testutil"
)

const loginTTL = time.Hour

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}

func createUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, login, role, plainPassword string) int64 {
	t.Helper()

	hash, err := password.Hash(plainPassword)
	require.NoError(t, err)

	var id int64
	err = pool.QueryRow(ctx, "INSERT INTO users (login, password_hash, role) VALUES ($1, $2, $3) RETURNING id",
		login, hash, role).Scan(&id)
	require.NoError(t, err, "create test user")

	return id
}

func TestLoginRoundtrip(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	userID := createUser(t, ctx, pool, "login-user", RoleParticipant, "right-password")

	token, user, err := service.Login(ctx, "  Login-User  ", "right-password", loginTTL)
	require.NoError(t, err)
	assert.Equal(t, User{ID: userID, Login: "login-user", Role: RoleParticipant}, user)
	assert.Len(t, token, 64, "32 random bytes hex-encoded")

	var storedHash, expiresAt string
	var storedUser int64
	err = pool.QueryRow(ctx,
		"SELECT token_hash, user_id, expires_at::text FROM sessions WHERE user_id = $1", userID).
		Scan(&storedHash, &storedUser, &expiresAt)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, hex.EncodeToString(sum[:]), storedHash, "the database stores the SHA-256 hash of the token")
	assert.NotEqual(t, token, storedHash, "no raw cookie token in the database")
	assert.Equal(t, userID, storedUser)

	loaded, err := service.User(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, user, loaded)

	require.NoError(t, service.Logout(ctx, token))
	err = pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE user_id = $1", userID).Scan(&storedHash)
	assert.Error(t, err, "logout must delete the session row")
	assert.NoError(t, service.Logout(ctx, token), "logout of a removed session is idempotent")
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	createUser(t, ctx, pool, "credentials-user", RoleAdmin, "right-password")

	for _, tc := range []struct {
		name     string
		login    string
		password string
	}{
		{"unknown login", "nobody-here", "right-password"},
		{"wrong password", "credentials-user", "wrong-password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := service.Login(ctx, tc.login, tc.password, loginTTL)
			assert.ErrorIs(t, err, ErrInvalidCredentials)
		})
	}

	var sessions int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	assert.Zero(t, sessions, "failed logins must not create sessions")
}

func TestUserRejectsUnknownAndExpiredSessions(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	userID := createUser(t, ctx, pool, "session-user", RoleParticipant, "password")

	_, err := service.User(ctx, "00000000000000000000000000000000")
	assert.ErrorIs(t, err, ErrNoSession, "unknown token")

	// A live session must coexist with the checks above.
	_, _, err = service.Login(ctx, "session-user", "password", loginTTL)
	require.NoError(t, err)

	// A substituted cookie with a well-formed but unknown token is still just
	// "no session".
	_, err = service.User(ctx, "11111111111111111111111111111111")
	assert.ErrorIs(t, err, ErrNoSession)

	expiredHash := sha256.Sum256([]byte("expired-token"))
	_, err = pool.Exec(ctx,
		"INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, now() - interval '1 second')",
		hex.EncodeToString(expiredHash[:]), userID)
	require.NoError(t, err)

	_, err = service.User(ctx, "expired-token")
	assert.ErrorIs(t, err, ErrNoSession, "server-side expiry check must reject expired sessions")

	// The expired row still exists: expiry filtering happens at read time.
	var rows int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&rows))
	assert.Equal(t, 2, rows)
}

func TestTwoLoginsIssueDistinctTokens(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)
	service := NewService(pool)

	createUser(t, ctx, pool, "rotation-user", RoleAdmin, "password")

	first, _, err := service.Login(ctx, "rotation-user", "password", loginTTL)
	require.NoError(t, err)
	second, _, err := service.Login(ctx, "rotation-user", "password", loginTTL)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "every successful login must issue a fresh token")
}
