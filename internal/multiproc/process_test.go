//go:build integration

package multiproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// replica is one real server process: HTTP and its own background worker
// goroutine on a unique port, logging into a captured line buffer.
type replica struct {
	name string
	port int
	base string

	cmd     *exec.Cmd
	done    chan struct{}
	exitErr error

	logs *lineBuffer
}

// replicaEnv assembles the process environment: the integration database
// from TEST_DATABASE_URL, a unique address, the shared test secrets and
// fast, bounded timeouts; the overrides adapt single scenarios.
func replicaEnv(port int, overrides map[string]string) []string {
	base := map[string]string{
		"DATABASE_URL":         os.Getenv("TEST_DATABASE_URL"),
		"HTTP_ADDR":            fmt.Sprintf("127.0.0.1:%d", port),
		"CSRF_SECRET":          testCSRFSecret,
		"SESSION_TTL":          "24h",
		"COOKIE_SECURE":        "false",
		"LOG_LEVEL":            "info",
		"METRICS_ENABLED":      "true",
		"DB_MAX_CONNS":         "4",
		"DB_TIMEOUT":           "2s",
		"BID_TX_TIMEOUT":       "5s",
		"SHUTDOWN_TIMEOUT":     "3s",
		"WORKER_POLL_INTERVAL": "30ms",
		"WORKER_BATCH_SIZE":    "100",
	}
	for name, value := range overrides {
		base[name] = value
	}

	owned := make(map[string]struct{}, len(base))
	for name := range base {
		owned[name] = struct{}{}
	}
	env := make([]string, 0, len(base)+8)
	for _, entry := range os.Environ() {
		if name, _, found := strings.Cut(entry, "="); found {
			if _, taken := owned[name]; taken {
				continue
			}
		}
		env = append(env, entry)
	}
	for name, value := range base {
		env = append(env, name+"="+value)
	}

	return env
}

// startReplica launches one server process and waits until /livez answers;
// the process is killed by the test cleanup if the scenario leaves it running.
func startReplica(t *testing.T, name string, overrides map[string]string) *replica {
	t.Helper()
	requireDatabaseURL(t)

	port := freePort(t)
	replica := &replica{
		name: name,
		port: port,
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		logs: newLineBuffer(),
	}

	cmd := exec.CommandContext(backgroundContext(), serverBinary) //nolint:noctx // the processes outlive single requests and are reaped by the tests
	cmd.Env = replicaEnv(port, overrides)
	cmd.Stdout = replica.logs
	cmd.Stderr = replica.logs
	require.NoError(t, cmd.Start(), "the server binary must start")
	replica.cmd = cmd

	done := make(chan struct{})
	replica.done = done
	go func() {
		replica.exitErr = cmd.Wait()
		close(done)
	}()

	replica.waitLive(t)
	t.Cleanup(replica.kill)

	return replica
}

// waitLive polls /livez until the process serves HTTP; a timeout carries the
// captured logs, which name the startup failure.
func (r *replica) waitLive(t *testing.T) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(backgroundContext(), http.MethodGet, r.base+"/livez", nil)
		if err == nil {
			resp, reqErr := client.Do(req) //nolint:bodyclose // the response is drained and closed right here
			if reqErr == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		select {
		case <-r.done:
			t.Fatalf("replica %s exited before serving: %v\nlogs:\n%s", r.name, r.exitErr, r.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica %s never answered /livez\nlogs:\n%s", r.name, r.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// isDone reports whether the process has already been reaped.
func (r *replica) isDone() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// kill terminates the process the hard way (TerminateProcess on Windows,
// SIGKILL elsewhere) and reaps it; it is safe to call several times.
func (r *replica) kill() {
	if r.isDone() {
		return
	}
	_ = r.cmd.Process.Kill()
	<-r.done
}

// exited confirms the process is gone and reports its exit code.
func (r *replica) exited(t *testing.T) int {
	t.Helper()

	select {
	case <-r.done:
	default:
		t.Fatalf("replica %s is still running", r.name)
	}
	if r.exitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(r.exitErr, &exitErr) {
		return exitErr.ExitCode()
	}

	return 1
}

// send delivers a signal to the process; used for the graceful-shutdown
// scenario on the platforms where signals exist.
func (r *replica) send(t *testing.T, sig os.Signal) {
	t.Helper()

	require.NoError(t, r.cmd.Process.Signal(sig))
}

// assertLogsAreClean proves the captured output is a stream of complete JSON
// events without secrets: no hang, no partially written record, no password
// or CSRF secret in any line.
func (r *replica) assertLogsAreClean(t *testing.T) {
	t.Helper()

	for _, line := range r.logs.lines() {
		if !json.Valid([]byte(line)) {
			t.Fatalf("replica %s logged a non-JSON record: %s", r.name, line)
		}
	}
	text := r.logs.String()
	assertNotContains(t, text, testCSRFSecret, "the CSRF secret must never be logged")
	if password := databasePassword(); password != "" {
		assertNotContains(t, text, password, "the database password must never be logged")
	}
}

// finishedLotIDs parses the committed completions of the process from its
// logs: the multi-process scenarios count them per replica.
func (r *replica) finishedLotIDs(t *testing.T) map[int64]int {
	t.Helper()

	finished := make(map[int64]int)
	for _, line := range r.logs.lines() {
		var record struct {
			Msg   string `json:"msg"`
			LotID int64  `json:"lot_id"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Msg == "lot finished" {
			finished[record.LotID]++
		}
	}

	return finished
}

// freePort reserves an ephemeral port and releases it for the process; a
// collision with another test's port is answered by a /livez timeout with
// the process logs.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(backgroundContext(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return port
}

// lineBuffer assembles written chunks into complete lines: the process pipes
// deliver arbitrary fragments, and the log checks need whole records.
type lineBuffer struct {
	mu       sync.Mutex
	pending  []byte
	complete []string
}

func newLineBuffer() *lineBuffer {
	return &lineBuffer{}
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, p...)
	for {
		index := bytes.IndexByte(b.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(b.pending[:index]), "\r")
		b.complete = append(b.complete, line)
		b.pending = b.pending[index+1:]
	}

	return len(p), nil
}

// lines returns the complete lines written so far; a pending fragment is
// never a record and stays out.
func (b *lineBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.complete...)
}

func (b *lineBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return strings.Join(b.complete, "\n")
}

func assertNotContains(t *testing.T, text, forbidden, why string) {
	t.Helper()

	if strings.Contains(text, forbidden) {
		t.Fatalf("%s: found %q in the output", why, forbidden)
	}
}

// databasePassword extracts the password part of TEST_DATABASE_URL for the
// log redaction checks; an empty URL yields an empty password.
func databasePassword() string {
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return ""
	}
	password, _ := parsed.User.Password()

	return password
}

// --- compose and server-side helpers ----------------------------------------

// runCompose runs one docker compose command of the test project from the
// repository root (the project of make test-integration).
func runCompose(t *testing.T, args ...string) {
	t.Helper()

	command := exec.CommandContext(backgroundContext(), "docker", append([]string{"compose"}, args...)...) //nolint:gosec,noctx // the arguments are fixed test commands, the invocation is bounded by the test
	command.Dir = repoRoot
	out, err := command.CombinedOutput()
	require.NoError(t, err, "docker compose %s failed: %s", strings.Join(args, " "), out)
}

// enableDeadClientDetection arms the test PostgreSQL to notice a crashed
// client while its backend waits on a lock (the PostgreSQL 14+
// client_connection_check_interval GUC): the abandoned finishing transaction
// is rolled back automatically, the way a managed production instance is
// configured. Without it a lock-waiting backend only notices the dead client
// when the wait resolves.
func enableDeadClientDetection(t *testing.T) {
	t.Helper()

	runCompose(t, "exec", "-T", "db-test", "psql", "-U", "auction", "-d", "auction_test", "-q",
		"-c", "ALTER SYSTEM SET client_connection_check_interval = '100ms'",
		"-c", "SELECT pg_reload_conf()")
}

// --- database barriers ------------------------------------------------------

// bidRowBarrier holds FOR UPDATE on one bid row: the worker's finishing
// UPDATE of the lot blocks on the foreign-key validation while the barrier
// holds, so a scenario controls exactly when the commit may happen.
type bidRowBarrier struct {
	ctx context.Context
	tx  pgx.Tx
}

// holdBidRow opens the barrier around one bid row; the lock lives until
// release (or the test cleanup).
func holdBidRow(t *testing.T, pool *pgxpool.Pool, bidID int64) *bidRowBarrier {
	t.Helper()

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	var lockedID int64
	require.NoError(t, tx.QueryRow(ctx, "SELECT id FROM bids WHERE id = $1 FOR UPDATE", bidID).Scan(&lockedID))

	barrier := &bidRowBarrier{ctx: ctx, tx: tx}
	t.Cleanup(barrier.release)

	return barrier
}

// release drops the barrier; the blocked finish transaction becomes able to
// commit.
func (b *bidRowBarrier) release() {
	_ = b.tx.Rollback(b.ctx)
}

// waitingLockCount reports the transactions currently blocked on a lock: the
// barrier scenarios use it as evidence that the worker really sits inside
// its finishing transaction.
func waitingLockCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()

	var count int64
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&count))

	return count
}

// lotRowUnlocked probes the lot row with FOR UPDATE NOWAIT in an implicit
// transaction: success proves no process holds the row any more (a crashed
// owner had its transaction rolled back by PostgreSQL).
func lotRowUnlocked(t *testing.T, pool *pgxpool.Pool, lotID int64) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var id int64
	err := pool.QueryRow(ctx, "SELECT id FROM lots WHERE id = $1 FOR UPDATE NOWAIT", lotID).Scan(&id)

	return err == nil
}

// dumpLotRowLocks prints the current lockers of the lots table for
// diagnostics.
func dumpLotRowLocks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	rows, err := pool.Query(context.Background(), `
		SELECT l.pid, l.locktype, l.mode, l.granted, a.state, a.query
		FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.relation = 'lots'::regclass
		ORDER BY l.granted, l.pid`)
	if err != nil {
		t.Logf("lock dump failed: %v", err)

		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		var locktype, mode, state, query string
		var granted bool
		if err := rows.Scan(&pid, &locktype, &mode, &granted, &state, &query); err != nil {
			t.Logf("lock dump scan failed: %v", err)

			return
		}
		t.Logf("locker pid=%d locktype=%s mode=%s granted=%t state=%s query=%s", pid, locktype, mode, granted, state, query)
	}
}
