//go:build integration

package multiproc

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// TestDatabaseOutageAndRecovery stops the backing PostgreSQL under a running
// replica: operations answer 503 within their bounded timeouts, /livez stays
// available while /readyz reports not-ready, the worker logs its failed pass
// and the process survives. After the database comes back — without any
// application restart — new requests and worker passes succeed again and the
// success gauge advances.
func TestDatabaseOutageAndRecovery(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Отказ базы данных")
	participantID := createUser(t, ctx, pool, "mp-outage-participant", "participant", "multiproc-participant")
	bidLot := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))
	deferredLot := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))
	insertBidRow(t, ctx, pool, deferredLot, participantID, 150, time.Now().Add(-time.Minute),
		"17000000-0000-4000-8000-000000000001")

	replica := startReplica(t, "outage", nil)
	participant := newClient(t)
	login(t, participant, replica.base, "mp-outage-participant", "multiproc-participant")
	token := pageToken(t, participant, replica.base+"/login")
	bidPath := replica.base + "/api/lots/" + strconv.FormatInt(bidLot, 10) + "/bids"

	// The worker succeeded before the outage: the gauge carries its pass.
	waitFor(t, 10*time.Second, "the first successful worker pass", func() bool {
		return workerSuccessGauge(t, replica.base) > 0
	})
	gaugeBefore := workerSuccessGauge(t, replica.base)

	// The backing service goes away: the resource is stoppable, the process
	// is disposable.
	runCompose(t, "stop", "db-test")

	outageStart := time.Now()
	resp := postJSON(t, participant, bidPath, bidAPIBody("110", "17000000-0000-4000-8000-000000000002"),
		map[string]string{"X-CSRF-Token": token})
	elapsed := time.Since(outageStart)
	require.Equal(t, http.StatusServiceUnavailable, resp.Status,
		"the operation must be refused while the database is down: %s", resp.Body)
	assert.Less(t, elapsed, 15*time.Second, "the refused operation must stay bounded")

	probe := get(t, participant, replica.base+"/livez")
	assert.Equal(t, http.StatusOK, probe.Status, "/livez never touches the database")
	ready := get(t, participant, replica.base+"/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, ready.Status, "/readyz must report not-ready")

	// The worker logs the failed pass and keeps the process alive.
	waitFor(t, 10*time.Second, "the failed worker pass in the logs", func() bool {
		for _, line := range replica.logs.lines() {
			if logField(line, "msg") == "worker pass failed" {
				return true
			}
		}

		return false
	})
	require.False(t, replica.isDone(), "a database outage must not stop the process")
	replica.assertLogsAreClean(t)

	// The database comes back; the application is never restarted.
	runCompose(t, "start", "db-test")
	waitFor(t, 60*time.Second, "the readiness probe to recover", func() bool {
		return get(t, participant, replica.base+"/readyz").Status == http.StatusOK
	})

	// New requests succeed again.
	resp = postJSON(t, participant, bidPath, bidAPIBody("110", "17000000-0000-4000-8000-000000000003"),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusCreated, resp.Status, "the bid must be accepted after recovery: %s", resp.Body)

	// The deferred lot is finished by the worker passes that run again.
	setLotDeadline(t, ctx, pool, deferredLot, time.Now().Add(-time.Second))
	state := waitLotFinished(t, ctx, pool, deferredLot)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, int64(150), maxBidAmount(t, ctx, pool, deferredLot))

	// The success gauge advanced past its pre-outage value.
	waitFor(t, 15*time.Second, "the success gauge to advance", func() bool {
		return workerSuccessGauge(t, replica.base) > gaugeBefore
	})

	replica.assertLogsAreClean(t)
}

// workerSuccessGauge reads auction_worker_last_success_timestamp_seconds from
// the /metrics exporter of a replica.
func workerSuccessGauge(t *testing.T, base string) float64 {
	t.Helper()

	resp := get(t, newClient(t), base+"/metrics")
	require.Equal(t, http.StatusOK, resp.Status, "/metrics must answer")
	const gauge = "auction_worker_last_success_timestamp_seconds"
	for _, line := range strings.Split(resp.Body, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), gauge+" "); found {
			parsed, err := strconv.ParseFloat(value, 64)
			require.NoError(t, err, "the gauge %s must carry a number", gauge)

			return parsed
		}
	}
	t.Fatalf("the gauge %s is absent from /metrics", gauge)

	return 0
}
