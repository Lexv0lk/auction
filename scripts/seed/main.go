// Command seed fills the database named by DATABASE_URL with the demo data
// set of the educational auction: one administrator, three participants,
// artifact categories and draft lots (see internal/seed and README.md).
//
// It is a separate one-off administrative process of the same release: the
// server never seeds or migrates automatically and has no seed subcommand.
// The command is a plain Go program, so make seed works the same way in
// PowerShell and Linux shells. Passwords arrive through the environment
// (SEED_ADMIN_PASSWORD, SEED_PARTICIPANT_PASSWORD) and are only stored as
// bcrypt hashes; they are never printed to stdout or error messages.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/postgres"
	"github.com/Lexv0lk/auction/internal/seed"
)

const (
	connectTimeout = 10 * time.Second
	seedTimeout    = 2 * time.Minute
)

var (
	errDatabaseURLRequired             = errors.New("DATABASE_URL is required; copy .env.example to .env or set the variable")
	errSeedAdminPasswordRequired       = errors.New("SEED_ADMIN_PASSWORD is required; see README.md for the demo accounts")
	errSeedParticipantPasswordRequired = errors.New("SEED_PARTICIPANT_PASSWORD is required; see README.md for the demo accounts")
)

type seedConfig struct {
	databaseURL string
	options     seed.Options
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()

	pool, err := postgres.Connect(ctx, config.Database{
		URL:      cfg.databaseURL,
		MaxConns: 2,
		Timeout:  connectTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	summary, err := seed.Run(ctx, pool, cfg.options)
	if err != nil {
		return err
	}

	fmt.Println(summary)

	return nil
}

func loadConfig(getenv func(string) string) (seedConfig, error) {
	cfg := seedConfig{
		databaseURL: getenv("DATABASE_URL"),
		options: seed.Options{
			AdminPassword:       getenv("SEED_ADMIN_PASSWORD"),
			ParticipantPassword: getenv("SEED_PARTICIPANT_PASSWORD"),
		},
	}

	if cfg.databaseURL == "" {
		return seedConfig{}, errDatabaseURLRequired
	}
	if cfg.options.AdminPassword == "" {
		return seedConfig{}, errSeedAdminPasswordRequired
	}
	if cfg.options.ParticipantPassword == "" {
		return seedConfig{}, errSeedParticipantPasswordRequired
	}

	return cfg, nil
}
