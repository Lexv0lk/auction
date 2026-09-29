package httpapp

import (
	"context"
	"crypto/sha256"
	"net/http"
	"time"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
)

// Config carries the authentication-related server settings. The CSRF key is
// derived once at startup: every replica that is given the same CSRF_SECRET
// derives the same 32-byte key, so masked tokens keep validating across
// replicas and restarts without any shared process state.
type Config struct {
	SessionTTL   time.Duration
	CookieSecure bool
	CSRFKey      []byte

	// MetricsEnabled gates the /metrics exporter; the environment decides
	// whether the process exposes it (METRICS_ENABLED).
	MetricsEnabled bool

	// Readiness performs the short database check behind /readyz: a pool
	// ping plus the schema version verification. The process is ready only
	// while it answers nil.
	Readiness func(ctx context.Context) error

	// Draining reports that the shutdown has started: a draining process
	// answers /readyz with 503 without touching the database.
	Draining func() bool
}

// Metrics is the observability contract of the HTTP layer: the exporter
// handler for /metrics and the instruments of the request and bid paths.
type Metrics interface {
	Handler() http.Handler
	ObserveHTTPRequest(method, route string, status int, duration time.Duration)
	CountBidAttempt(outcome, reason string)
	ObserveBidTransaction(duration time.Duration)
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

// Lots is the lot-service contract the HTTP layer depends on. Only the
// editable fields travel into the service: status and result fields are
// decided by the lot operations themselves. The participant reads never
// return drafts, and every bid goes through the single PlaceBid operation
// with the participant identity taken from the session.
type Lots interface {
	List(ctx context.Context) ([]lot.Lot, error)
	Get(ctx context.Context, id int64) (lot.Lot, error)
	Create(ctx context.Context, input lot.Input) (lot.Lot, error)
	Update(ctx context.Context, id int64, input lot.Input) (lot.Lot, error)
	Delete(ctx context.Context, id int64) error
	Publish(ctx context.Context, id int64) (lot.Lot, error)
	Catalog(ctx context.Context, filter lot.CatalogFilter) ([]lot.CatalogItem, bool, error)
	GetPublicLot(ctx context.Context, id int64) (lot.PublicLot, error)
	ListBids(ctx context.Context, lotID int64, page int) ([]lot.Bid, bool, error)
	PlaceBid(ctx context.Context, participantID, lotID int64, amount int64, requestKey string) (lot.PlacedBid, error)
}

// NewCSRFKey derives the CSRF signing key from the configured secret. Hashing
// normalizes a secret of any length to exactly the 32 bytes the CSRF library
// requires.
func NewCSRFKey(secret string) []byte {
	key := sha256.Sum256([]byte(secret))

	return key[:]
}
