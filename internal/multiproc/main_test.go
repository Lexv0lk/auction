//go:build integration

package multiproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The scenarios run real server processes: the binary is built once per test
// run from the same working tree, then every scenario starts it with its own
// environment and port.
var (
	repoRoot     string
	serverBinary string
)

// testCSRFSecret is the shared CSRF_SECRET of every replica started here:
// at least 32 characters and never a production value. The log checks prove
// it never appears in the process output.
const testCSRFSecret = "multiproc-integration-csrf-secret-0123456789abcdef"

var (
	errGoModNotFound = errors.New("go.mod not found above the working directory")
	errNoDatabaseURL = errors.New("TEST_DATABASE_URL is not set; run integration tests through make test-integration")
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	repoRoot = root

	if os.Getenv("TEST_DATABASE_URL") == "" {
		fmt.Fprintln(os.Stderr, errNoDatabaseURL)

		return 1
	}

	binary, cleanup, err := buildServer(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	serverBinary = binary
	defer cleanup()

	return m.Run()
}

// findRepoRoot walks up from the working directory (the package directory
// under go test) until it finds go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errGoModNotFound
		}
		dir = parent
	}
}

// buildServer compiles the server binary into a temporary directory once per
// test run: every scenario then starts the same artifact. The returned
// cleanup removes the temporary directory.
func buildServer(root string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "auction-multiproc-")
	if err != nil {
		return "", nil, fmt.Errorf("temp directory: %w", err)
	}
	binary := filepath.Join(dir, "server"+binarySuffix())
	command := exec.CommandContext(backgroundContext(), "go", "build", "-o", binary, "./cmd/server") //nolint:gosec // the build runs the project's own toolchain on a fixed package path
	command.Dir = root
	if err = command.Run(); err != nil {
		_ = os.RemoveAll(dir)

		return "", nil, fmt.Errorf("go build ./cmd/server: %w", err)
	}

	return binary, func() { _ = os.RemoveAll(dir) }, nil
}

// binarySuffix names the artifact so Windows can execute it.
func binarySuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}

	return ""
}

// requireDatabaseURL guards the helpers that only make sense against the
// dedicated integration database.
func requireDatabaseURL(t *testing.T) {
	t.Helper()

	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Fatal(errNoDatabaseURL)
	}
}

// backgroundContext is the process-level context of the multiproc helpers:
// server processes and their probes outlive any single request and are
// reaped explicitly by the tests.
func backgroundContext() context.Context {
	return context.Background()
}
