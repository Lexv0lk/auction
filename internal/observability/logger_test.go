package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errFakeDriverMessage = errors.New("postgres://auction:hunter2@localhost:15432/auction")

func TestRedactSecretsMasksCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		input    string
		expected string
	}{
		"postgres URL password": {
			input:    "connect database: postgres://auction:hunter2@localhost:15432/auction?sslmode=disable",
			expected: "connect database: postgres://auction:[redacted]@localhost:15432/auction?sslmode=disable",
		},
		"keyword password parameter": {
			input:    "connect failed: host=localhost user=auction password=hunter2 database=auction",
			expected: "connect failed: host=localhost user=auction password=[redacted] database=auction",
		},
		"plain text stays untouched": {
			input:    "http server started addr=127.0.0.1:8080",
			expected: "http server started addr=127.0.0.1:8080",
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.expected, RedactSecrets(tc.input))
		})
	}
}

func TestLoggerRedactsSecretsInStringsAndErrors(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, slog.LevelInfo)

	logger.Info("connect database", "error", errFakeDriverMessage)
	logger.Info("csrf key derived", "secret", "hunter2-secret-value")

	line := out.String()
	require.True(t, strings.Contains(line, "connect database"), "the events must stay in the log")

	var events []map[string]any
	for _, record := range strings.Split(strings.TrimSpace(line), "\n") {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(record), &event))
		events = append(events, event)
	}
	assert.Equal(t, "postgres://auction:[redacted]@localhost:15432/auction", events[0]["error"])
	assert.Equal(t, "[redacted]", events[1]["secret"])
}
