package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelpNeedsNoConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CSRF_SECRET", "")
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, run([]string{arg}, &out))
			assert.Contains(t, out.String(), "Usage: server")
		})
	}
}

func TestNoArgumentsLoadsApplicationConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var out bytes.Buffer
	err := run(nil, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "DATABASE_URL is required")
}

func TestRejectsProfilesAndUnknownFlags(t *testing.T) {
	for _, arg := range []string{"serve", "worker", "migrate", "seed", "--unknown"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			assert.Error(t, run([]string{arg}, &out))
		})
	}
}
