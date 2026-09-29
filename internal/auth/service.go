package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Lexv0lk/auction/internal/password"
)

// maxLoginLength bounds the account name accepted from a login form; the
// form offers the same limit.
const maxLoginLength = 64

// sessionTokenBytes is the entropy of the opaque session token (32 random
// bytes, hex-encoded into a 64-character cookie value).
const sessionTokenBytes = 32

var (
	// ErrInvalidCredentials is the single user-visible outcome for an unknown
	// login, a wrong password and malformed fields alike, so responses never
	// reveal whether an account exists.
	ErrInvalidCredentials = errors.New("invalid login or password")
	// ErrNoSession means the token is unknown, expired or substituted;
	// handlers treat it as "not logged in", not as a technical failure.
	ErrNoSession = errors.New("session is missing or expired")
)

// Pool is the subset of the connection pool the session service needs.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Expiry and session identity are computed by the database clock (now()), so
// replicas and restarts share one notion of time.
const (
	selectUserByLoginSQL = "SELECT id, role, password_hash FROM users WHERE login = $1"
	insertSessionSQL     = "INSERT INTO sessions (token_hash, user_id, expires_at)" +
		" VALUES ($1, $2, now() + make_interval(secs => $3::double precision))"
	selectSessionUserSQL = "SELECT u.id, u.login, u.role" +
		" FROM sessions s JOIN users u ON u.id = s.user_id" +
		" WHERE s.token_hash = $1 AND s.expires_at > now()"
	deleteSessionSQL = "DELETE FROM sessions WHERE token_hash = $1"
)

// dummyPasswordHash keeps unknown-login attempts on the same bcrypt path as
// wrong-password attempts; its plaintext is a throwaway value, not a secret.
const dummyPasswordHash = "$2a$10$ei1DGGYqMeIUA9UHkYDSNeYuOWKfKtqufKj0lskxMvlbd1a5g1an." //nolint:gosec // G101 // not a credential: a hash of a throwaway string used only to equalize timing

// Service implements login, per-request session validation and logout against
// the shared PostgreSQL pool.
type Service struct {
	pool Pool
}

// NewService builds the session service on top of the shared connection pool.
func NewService(pool Pool) *Service {
	return &Service{pool: pool}
}

// Login verifies the credentials and creates a fresh session: a new random
// token is issued on every successful login (the previous cookie value is
// never reused), the database stores only its SHA-256 hash next to the user
// and the database-clock expiry. Unknown login and wrong password return the
// same ErrInvalidCredentials after comparable bcrypt work.
func (s *Service) Login(ctx context.Context, login, plain string, ttl time.Duration) (string, User, error) {
	login = NormalizeLogin(login)
	if login == "" || len(login) > maxLoginLength || plain == "" || len(plain) > password.MaxBytes {
		return "", User{}, ErrInvalidCredentials
	}

	var user User
	var storedHash string
	err := s.pool.QueryRow(ctx, selectUserByLoginSQL, login).Scan(&user.ID, &user.Role, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = password.Verify(dummyPasswordHash, plain)

		return "", User{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", User{}, fmt.Errorf("look up user: %w", err)
	}
	if err := password.Verify(storedHash, plain); err != nil {
		return "", User{}, ErrInvalidCredentials
	}
	user.Login = login

	token, err := newToken()
	if err != nil {
		return "", User{}, fmt.Errorf("generate session token: %w", err)
	}
	if _, err := s.pool.Exec(ctx, insertSessionSQL, tokenHash(token), user.ID, ttl.Seconds()); err != nil {
		return "", User{}, fmt.Errorf("store session: %w", err)
	}

	return token, user, nil
}

// User loads the account of a valid, unexpired session. Unknown and expired
// tokens both return ErrNoSession; database failures are wrapped errors and
// must not be presented to users as "wrong password".
func (s *Service) User(ctx context.Context, token string) (User, error) {
	var user User
	err := s.pool.QueryRow(ctx, selectSessionUserSQL, tokenHash(token)).Scan(&user.ID, &user.Login, &user.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("load session user: %w", err)
	}

	return user, nil
}

// Logout deletes the session row and is idempotent: logging out with an
// already expired or unknown token is not an error.
func (s *Service) Logout(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, deleteSessionSQL, tokenHash(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	return nil
}

func newToken() (string, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}
