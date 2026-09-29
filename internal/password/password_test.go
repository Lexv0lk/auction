package password_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/Lexv0lk/auction/internal/password"
)

// The hash format is part of the release contract: hashes stored by the seed
// script must stay verifiable by the login code of later steps. The cost is
// pinned to bcrypt.DefaultCost (10).
func TestHashUsesBcryptFormatWithPinnedCost(t *testing.T) {
	hash, err := password.Hash("correct horse battery staple")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(hash, "$2a$10$"), "hash %q must be a $2a$ bcrypt hash with cost 10", hash)

	cost, err := bcrypt.Cost([]byte(hash))
	require.NoError(t, err)
	assert.Equal(t, 10, cost)
}

func TestVerifyAcceptsConfiguredPasswordAndRejectsWrongOne(t *testing.T) {
	hash, err := password.Hash("correct horse battery staple")
	require.NoError(t, err)

	assert.NoError(t, password.Verify(hash, "correct horse battery staple"))
	assert.Error(t, password.Verify(hash, "wrong password"))
}

// The login code of step 05 verifies with golang.org/x/crypto/bcrypt; a hash
// produced here must be accepted by that function directly.
func TestHashIsVerifiedByBcryptLibraryDirectly(t *testing.T) {
	hash, err := password.Hash("demo password")
	require.NoError(t, err)

	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("demo password")))
	assert.ErrorIs(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("other password")),
		bcrypt.ErrMismatchedHashAndPassword)
}

// Every password needs its own random salt, so identical passwords never
// produce identical stored hashes.
func TestHashIsSaltedPerCall(t *testing.T) {
	first, err := password.Hash("same password")
	require.NoError(t, err)
	second, err := password.Hash("same password")
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.NoError(t, password.Verify(first, "same password"))
	assert.NoError(t, password.Verify(second, "same password"))
}

// A stored hash from an earlier release must stay verifiable; the fixed hash
// below was generated once with the same library and pins the format.
func TestVerifyAcceptsHashFromEarlierRelease(t *testing.T) {
	stored := "$2a$10$Ryg8wJlQ12codqQswKf/Neovwp6pwVF.ns3Xdk3YYmfI/TTTbXM4y"

	assert.NoError(t, password.Verify(stored, "P@ssw0rd"))
	assert.Error(t, password.Verify(stored, "P@ssw0rd "))
}

func TestHashRejectsEmptyPassword(t *testing.T) {
	_, err := password.Hash("")

	assert.ErrorIs(t, err, password.ErrEmptyPassword)
}

func TestHashRejectsPasswordsLongerThanSeventyTwoBytes(t *testing.T) {
	_, err := password.Hash(strings.Repeat("a", 73))
	assert.ErrorIs(t, err, password.ErrPasswordTooLong)

	_, err = password.Hash(strings.Repeat("a", 72))
	assert.NoError(t, err, "exactly 72 bytes are accepted")

	_, err = password.Hash(strings.Repeat("Ω", 40))
	assert.ErrorIs(t, err, password.ErrPasswordTooLong, "bytes are counted, not runes")
}

// Error values must never carry the password itself.
func TestHashErrorsDoNotContainThePassword(t *testing.T) {
	secret := "secret demo password " + strings.Repeat("x", 60)
	_, err := password.Hash(secret)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "secret demo password")
}

func TestVerifyRejectsInvalidHash(t *testing.T) {
	err := password.Verify("not-a-bcrypt-hash", "any password")
	require.Error(t, err)
	assert.False(t, errors.Is(err, bcrypt.ErrMismatchedHashAndPassword), "an invalid hash is not a password mismatch")
}
