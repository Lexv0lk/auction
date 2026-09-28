// Package config validates process settings supplied through the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	errDatabaseURLRequired     = errors.New("DATABASE_URL is required")
	errCSRFSecretTooShort      = errors.New("CSRF_SECRET must contain at least 32 characters")
	errInvalidDatabaseURL      = errors.New("DATABASE_URL must be a PostgreSQL URL with host and database name")
	errInvalidDatabaseURLQuery = errors.New("DATABASE_URL has an invalid query string")
	errInvalidLogLevel         = errors.New("LOG_LEVEL must be debug, info, warn or error")
	errInvalidPositiveInteger  = errors.New("must be a positive integer")
	errInvalidPositiveDuration = errors.New("must be a positive duration (for example 5s)")
	errInvalidBoolean          = errors.New("must be a boolean")
	errInvalidAddress          = errors.New("must be a host:port address")
	errInvalidPort             = errors.New("port must be between 1 and 65535")
)

// Config contains the settings for the server and its background processing.
type Config struct {
	Database        Database
	HTTP            HTTP
	Session         Session
	Worker          Worker
	LogLevel        slog.Level
	BidTxTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// Database configures the PostgreSQL connection pool and operation deadline.
type Database struct {
	URL      string
	MaxConns int
	Timeout  time.Duration
}

// HTTP configures the application's only HTTP listener.
type HTTP struct {
	Addr         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

// Session configures login sessions and CSRF protection.
type Session struct {
	TTL          time.Duration
	CookieSecure bool
	CSRFSecret   string
}

// Worker configures the background auction completion loop inside the server.
type Worker struct {
	PollInterval time.Duration
	BatchSize    int
}

// Load reads and validates the application's configuration from the environment.
func Load() (Config, error) {
	c := Config{}
	var err error
	c.Database.URL = os.Getenv("DATABASE_URL")
	if c.Database.URL == "" {
		return c, errDatabaseURLRequired
	}
	if err = validateDatabaseURL(c.Database.URL); err != nil {
		return c, err
	}
	c.LogLevel, err = parseLevel(getEnv("LOG_LEVEL", "info"))
	if err != nil {
		return c, err
	}
	c.Database.MaxConns, err = getInt("DB_MAX_CONNS", "10")
	if err != nil {
		return c, err
	}
	c.Database.Timeout, err = getDuration("DB_TIMEOUT", "5s")
	if err != nil {
		return c, err
	}
	c.ShutdownTimeout, err = getDuration("SHUTDOWN_TIMEOUT", "10s")
	if err != nil {
		return c, err
	}

	c.HTTP.Addr, err = getAddress("HTTP_ADDR", "127.0.0.1:8080")
	if err != nil {
		return c, err
	}
	c.HTTP.ReadTimeout, err = getDuration("HTTP_READ_TIMEOUT", "10s")
	if err != nil {
		return c, err
	}
	c.HTTP.WriteTimeout, err = getDuration("HTTP_WRITE_TIMEOUT", "15s")
	if err != nil {
		return c, err
	}
	c.HTTP.IdleTimeout, err = getDuration("HTTP_IDLE_TIMEOUT", "60s")
	if err != nil {
		return c, err
	}
	c.BidTxTimeout, err = getDuration("BID_TX_TIMEOUT", "5s")
	if err != nil {
		return c, err
	}
	c.Session.TTL, err = getDuration("SESSION_TTL", "24h")
	if err != nil {
		return c, err
	}
	c.Session.CookieSecure, err = getBoolean("COOKIE_SECURE", "false")
	if err != nil {
		return c, err
	}
	c.Session.CSRFSecret = os.Getenv("CSRF_SECRET")
	if len(c.Session.CSRFSecret) < 32 {
		return c, errCSRFSecretTooShort
	}
	c.Worker.PollInterval, err = getDuration("WORKER_POLL_INTERVAL", "1s")
	if err != nil {
		return c, err
	}
	c.Worker.BatchSize, err = getInt("WORKER_BATCH_SIZE", "100")
	if err != nil {
		return c, err
	}

	return c, nil
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}

func validateDatabaseURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
		return errInvalidDatabaseURL
	}
	if _, err := url.QueryUnescape(u.RawQuery); err != nil {
		return errInvalidDatabaseURLQuery
	}

	return nil
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errInvalidLogLevel
	}
}

func getInt(key, fallback string) (int, error) {
	n, err := strconv.Atoi(getEnv(key, fallback))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %w", key, errInvalidPositiveInteger)
	}

	return n, nil
}

func getDuration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(getEnv(key, fallback))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %w", key, errInvalidPositiveDuration)
	}

	return d, nil
}

func getBoolean(key, fallback string) (bool, error) {
	b, err := strconv.ParseBool(getEnv(key, fallback))
	if err != nil {
		return false, fmt.Errorf("%s %w", key, errInvalidBoolean)
	}

	return b, nil
}

func getAddress(key, fallback string) (string, error) {
	v := getEnv(key, fallback)
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("%s %w", key, errInvalidAddress)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%s %w", key, errInvalidPort)
	}

	return v, nil
}
