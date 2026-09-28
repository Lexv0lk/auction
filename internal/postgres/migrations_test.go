package postgres_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/postgres"
)

// The server requires an exact schema version, so the constant and the files
// in migrations/ must always describe the same release.
func TestMigrationsMatchExpectedSchemaVersion(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	require.NoError(t, err)

	upNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			require.True(t, strings.HasSuffix(name, ".down.sql"), "unexpected file %q in migrations", name)

			continue
		}
		upNames = append(upNames, name)
	}

	numbers := make(map[int]bool, len(upNames))
	for _, name := range upNames {
		number := migrationNumber(t, name)
		numbers[number] = true

		downName := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		_, err := os.Stat(filepath.Join("..", "..", "migrations", downName))
		assert.NoError(t, err, "down migration %q is missing", downName)
	}

	assert.Len(t, numbers, int(postgres.ExpectedSchemaVersion))
	for number := 1; number <= int(postgres.ExpectedSchemaVersion); number++ {
		assert.True(t, numbers[number], "migration number %d is missing", number)
	}
}

func migrationNumber(t *testing.T, name string) int {
	t.Helper()

	require.Regexp(t, `^\d{6}_`, name, "migration %q must start with a six-digit version and a separator", name)
	parsed, err := strconv.Atoi(name[:6])
	require.NoError(t, err)

	return parsed
}
