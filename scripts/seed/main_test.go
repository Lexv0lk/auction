package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/seed"
)

func envReader(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func TestLoadConfigReadsSeedEnvironment(t *testing.T) {
	config, err := loadConfig(envReader(map[string]string{
		"DATABASE_URL":              "postgres://auction:x@localhost:5432/auction",
		"SEED_ADMIN_PASSWORD":       "admin",
		"SEED_PARTICIPANT_PASSWORD": "participant",
	}))
	require.NoError(t, err)

	assert.Equal(t, "postgres://auction:x@localhost:5432/auction", config.databaseURL)
	assert.Equal(t, seed.Options{AdminPassword: "admin", ParticipantPassword: "participant"}, config.options)
}

func TestLoadConfigRejectsMissingVariables(t *testing.T) {
	full := map[string]string{
		"DATABASE_URL":              "postgres://auction:x@localhost:5432/auction",
		"SEED_ADMIN_PASSWORD":       "admin",
		"SEED_PARTICIPANT_PASSWORD": "participant",
	}

	cases := []struct {
		name    string
		missing string
	}{
		{"database URL", "DATABASE_URL"},
		{"admin password", "SEED_ADMIN_PASSWORD"},
		{"participant password", "SEED_PARTICIPANT_PASSWORD"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := map[string]string{}
			for key, value := range full {
				env[key] = value
			}
			delete(env, testCase.missing)

			_, err := loadConfig(envReader(env))

			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.missing)
			for _, password := range []string{full["SEED_ADMIN_PASSWORD"], full["SEED_PARTICIPANT_PASSWORD"]} {
				assert.NotContains(t, err.Error(), password)
			}
		})
	}
}
