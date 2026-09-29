package lot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRequestKeyIssuesCanonicalUniqueUUIDs(t *testing.T) {
	issued := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		key := NewRequestKey()
		require.True(t, validRequestKey(key), "issued key %q must be a canonical UUID", key)
		issued[key] = struct{}{}
	}

	assert.Len(t, issued, 1000, "every issued key is unique")
}
