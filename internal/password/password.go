// Package password owns the pinned password hashing contract: bcrypt with a
// random per-password salt, cost 10 (bcrypt.DefaultCost) and the $2a$ hash
// format produced by golang.org/x/crypto/bcrypt. The seed script stores hashes
// created here and the login code (step 05) verifies them with the same
// library, so the format and parameters must not change silently.
package password

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcrypt processes at most 72 bytes; longer inputs are rejected instead of
// being silently truncated.
const maxPasswordBytes = 72

// MaxBytes is the exported bcrypt input limit, so callers can reject oversized
// input before spending any database or hashing work.
const MaxBytes = maxPasswordBytes

var (
	// ErrEmptyPassword means a zero-length password was rejected.
	ErrEmptyPassword = errors.New("password must not be empty")
	// ErrPasswordTooLong means the password exceeds the bcrypt input limit.
	ErrPasswordTooLong = errors.New("password is longer than the supported 72 bytes")
)

// Hash derives the stored bcrypt hash of the password. Every call applies a
// fresh random salt, so equal passwords never yield equal hashes.
func Hash(plain string) (string, error) {
	if len(plain) == 0 {
		return "", ErrEmptyPassword
	}
	if len(plain) > maxPasswordBytes {
		return "", fmt.Errorf("%w: got %d bytes", ErrPasswordTooLong, len(plain))
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("generate bcrypt hash: %w", err)
	}

	return string(hash), nil
}

// Verify reports whether the password matches the stored hash. A nil error
// means the password is accepted; any error rejects the login attempt.
func Verify(hash, plain string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}
