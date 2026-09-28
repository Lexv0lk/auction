package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadApplication(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:private@localhost:5432/auction")
	t.Setenv("CSRF_SECRET", strings.Repeat("x", 32))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8765")
	t.Setenv("HTTP_READ_TIMEOUT", "2s")
	t.Setenv("WORKER_POLL_INTERVAL", "3s")
	t.Setenv("WORKER_BATCH_SIZE", "50")
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8765", c.HTTP.Addr)
	assert.Equal(t, 2*time.Second, c.HTTP.ReadTimeout)
	assert.Equal(t, 3*time.Second, c.Worker.PollInterval)
	assert.Equal(t, 50, c.Worker.BatchSize)
}

func TestLoadRejectsInvalidValuesWithoutLeakingSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:private@localhost:5432/auction")
	t.Setenv("CSRF_SECRET", strings.Repeat("x", 32))
	t.Setenv("HTTP_READ_TIMEOUT", "0s")
	_, err := Load()
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP_READ_TIMEOUT")
	assert.NotContains(t, err.Error(), "private")
	t.Setenv("HTTP_READ_TIMEOUT", "10s")
	t.Setenv("DATABASE_URL", "private")
	_, err = Load()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private")
}

func TestApplicationValidatesBackgroundSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:private@localhost:5432/auction")
	t.Setenv("CSRF_SECRET", strings.Repeat("x", 32))
	for _, tt := range []struct{ key, value string }{
		{"WORKER_POLL_INTERVAL", "invalid"},
		{"WORKER_POLL_INTERVAL", "0s"},
		{"WORKER_BATCH_SIZE", "-1"},
		{"CSRF_SECRET", ""},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			_, err := Load()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.key)
		})
	}
	// The removed worker listener has no effect on the single server configuration.
	t.Setenv("WORKER_HTTP_ADDR", "invalid")
	_, err := Load()
	assert.NoError(t, err, "obsolete worker listener affected configuration")
}
