package httpapp

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
)

// Config carries the authentication-related server settings. The CSRF key is
// derived once at startup: every replica that is given the same CSRF_SECRET
// derives the same 32-byte key, so masked tokens keep validating across
// replicas and restarts without any shared process state.
type Config struct {
	SessionTTL   time.Duration
	CookieSecure bool
	CSRFKey      []byte
}

// Authenticator is the session-service contract the HTTP layer depends on.
// The application identity of a request always comes from this service, never
// from client-supplied form fields, JSON bodies or headers.
type Authenticator interface {
	Login(ctx context.Context, login, password string, ttl time.Duration) (token string, user auth.User, err error)
	User(ctx context.Context, token string) (auth.User, error)
	Logout(ctx context.Context, token string) error
}

// Categories is the reference-data contract the admin handlers depend on.
type Categories interface {
	List(ctx context.Context) ([]category.Category, error)
	Create(ctx context.Context, name string) (category.Category, error)
	Get(ctx context.Context, id int64) (category.Category, error)
	Rename(ctx context.Context, id int64, name string) (category.Category, error)
	Delete(ctx context.Context, id int64) error
}

// NewCSRFKey derives the CSRF signing key from the configured secret. Hashing
// normalizes a secret of any length to exactly the 32 bytes the CSRF library
// requires.
func NewCSRFKey(secret string) []byte {
	key := sha256.Sum256([]byte(secret))

	return key[:]
}
