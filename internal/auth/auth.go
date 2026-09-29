// Package auth owns login sessions stored in PostgreSQL: password checks via
// the pinned bcrypt contract of internal/password, opaque random tokens that
// only travel in the session cookie, and their SHA-256 hashes persisted in the
// sessions table. The process itself keeps no session state, so any server
// replica can validate any request after a restart.
package auth

import (
	"context"
	"strings"
)

// Fixed account roles of the educational auction.
const (
	RoleAdmin       = "admin"
	RoleParticipant = "participant"
)

// User is the authenticated account loaded from the database, never from
// client-supplied fields.
type User struct {
	ID    int64
	Login string
	Role  string
}

type userContextKey struct{}

// WithUser stores the authenticated user in the request context.
func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, userContextKey{}, user)
}

// UserFromContext returns the authenticated user stored in the context, if any.
func UserFromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey{}).(User)

	return user, ok
}

// NormalizeLogin applies the account-name normalization shared with the
// migration contract: surrounding spaces removed and case folded, the form
// every login is stored in.
func NormalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}
