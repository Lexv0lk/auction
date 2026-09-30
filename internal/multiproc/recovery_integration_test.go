//go:build integration

package multiproc

import (
	"net/http"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// TestReplicaReplacementPreservesState kills a running replica and replaces
// it with a fresh process: the session and the bid history survive (they live
// in PostgreSQL), the replacement serves requests and its background loop
// finishes the accumulated auction.
func TestReplicaReplacementPreservesState(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Замена реплики")
	createUser(t, ctx, pool, "mp-replace-participant", "participant", "multiproc-participant")
	lotID := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))

	original := startReplica(t, "original", nil)
	participant := newClient(t)
	login(t, participant, original.base, "mp-replace-participant", "multiproc-participant")
	token := pageToken(t, participant, original.base+"/login")
	bidPath := original.base + "/api/lots/" + strconv.FormatInt(lotID, 10) + "/bids"

	resp := postJSON(t, participant, bidPath, bidAPIBody("100", "13000000-0000-4000-8000-000000000001"),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusCreated, resp.Status)
	resp = postJSON(t, participant, bidPath, bidAPIBody("101", "13000000-0000-4000-8000-000000000002"),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusCreated, resp.Status)
	require.Equal(t, int64(2), countBids(t, ctx, pool, lotID))

	// The replica is replaced: a hard kill of the process, then a fresh one.
	original.kill()
	original.exited(t)

	replacement := startReplica(t, "replacement", nil)
	lotPage := get(t, participant, replacement.base+"/lots/"+strconv.FormatInt(lotID, 10))
	require.Equal(t, http.StatusOK, lotPage.Status, "the session must survive the replacement")
	assert.Contains(t, lotPage.Body, ">100<", "the bid history must survive the replacement")
	assert.Contains(t, lotPage.Body, ">101<")

	// The same client keeps acting on the replacement.
	resp = postJSON(t, participant, replacement.base+"/api/lots/"+strconv.FormatInt(lotID, 10)+"/bids",
		bidAPIBody("102", "13000000-0000-4000-8000-000000000003"),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusCreated, resp.Status, "the session must stay valid: %s", resp.Body)

	// The replacement's background loop resumes the passes: the accumulated
	// auction is finished by the new process alone.
	setLotDeadline(t, ctx, pool, lotID, time.Now().Add(-time.Second))
	state := waitLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, int64(102), maxBidAmount(t, ctx, pool, lotID))

	replacement.assertLogsAreClean(t)
	original.assertLogsAreClean(t)
}

// TestAccumulatedAuctionFinishedAfterStartup runs no process while a deadline
// passes: after the start the HTTP side already refuses the late bids, and
// the first background pass of the fresh process finishes the accumulated
// auction on its own.
func TestAccumulatedAuctionFinishedAfterStartup(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Накопленные торги")
	participantID := createUser(t, ctx, pool, "mp-accumulated-participant", "participant", "multiproc-participant")
	lotID := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Second))
	bidID := insertBidRow(t, ctx, pool, lotID, participantID, 150, time.Now().Add(-2*time.Minute),
		"14000000-0000-4000-8000-000000000001")

	replica := startReplica(t, "after-gap", nil)
	participant := newClient(t)
	login(t, participant, replica.base, "mp-accumulated-participant", "multiproc-participant")
	token := pageToken(t, participant, replica.base+"/login")

	resp := postJSON(t, participant, replica.base+"/api/lots/"+strconv.FormatInt(lotID, 10)+"/bids",
		bidAPIBody("180", "14000000-0000-4000-8000-000000000002"),
		map[string]string{"X-CSRF-Token": token})
	require.Equal(t, http.StatusConflict, resp.Status, "the late bid must be refused")
	assert.Contains(t, resp.Body, "auction_closed")
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID), "no late bid is written")

	state := waitLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, bidID, *state.winningBidID, "the accumulated bid wins")

	winnerPage := get(t, participant, replica.base+"/lots/"+strconv.FormatInt(lotID, 10))
	require.Equal(t, http.StatusOK, winnerPage.Status)
	assert.Contains(t, winnerPage.Body, "Победитель")
	assert.Contains(t, winnerPage.Body, ">150<")

	replica.assertLogsAreClean(t)
}

// TestKillDuringWorkerPassLeavesNoPartialResult crashes a replica while its
// background transaction sits blocked inside the finishing UPDATE: the
// crashed process leaves no partial result, PostgreSQL rolls the abandoned
// transaction back, and a successor replica finishes the lot. A crash after
// the commit changes nothing — the finished lot is never re-processed.
func TestKillDuringWorkerPassLeavesNoPartialResult(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Аварийная остановка")
	participantID := createUser(t, ctx, pool, "mp-crash-participant", "participant", "multiproc-participant")
	lotID := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Second))
	winnerID := insertBidRow(t, ctx, pool, lotID, participantID, 150, time.Now().Add(-2*time.Minute),
		"15000000-0000-4000-8000-000000000001")

	// The barrier holds the winning bid row: the worker's UPDATE of the lot
	// blocks on the foreign-key validation, so the crash lands inside the
	// finishing transaction, before its commit.
	enableDeadClientDetection(t)
	barrier := holdBidRow(t, pool, winnerID)
	crashed := startReplica(t, "crashed", nil)
	waitFor(t, 15*time.Second, "the worker pass to block on the barrier", func() bool {
		return waitingLockCount(t, pool) > 0
	})
	require.Equal(t, lot.StatusActive, readLotState(t, ctx, pool, lotID).status,
		"the blocked pass must not have committed anything")

	crashed.kill()
	crashed.exited(t)

	// PostgreSQL rolled the dead process's transaction back: the lot row is
	// free and no partial result exists.
	debugDumped := false
	waitFor(t, 10*time.Second, "PostgreSQL to release the crashed transaction's locks", func() bool {
		unlocked := lotRowUnlocked(t, pool, lotID)
		if !unlocked && !debugDumped {
			debugDumped = true
			dumpLotRowLocks(t, pool)
		}

		return unlocked
	})
	state := readLotState(t, ctx, pool, lotID)
	assert.Equal(t, lot.StatusActive, state.status, "the crash must not leave a partial result")
	assert.Nil(t, state.winningBidID)
	assert.Nil(t, state.finishedAt)
	assert.Equal(t, int64(1), countBids(t, ctx, pool, lotID))
	assert.Equal(t, int64(150), maxBidAmount(t, ctx, pool, lotID))

	// The successor replica picks the unfinished lot up.
	barrier.release()
	successor := startReplica(t, "successor", nil)
	finished := waitLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, finished.winningBidID)
	assert.Equal(t, winnerID, *finished.winningBidID)

	// A crash after the commit: the recorded result is final and survives
	// yet another replacement.
	successor.kill()
	successor.exited(t)
	afterFinish := startReplica(t, "after-finish", nil)
	waitFor(t, 10*time.Second, "the worker pass of the replacement", func() bool {
		for _, line := range afterFinish.logs.lines() {
			if logField(line, "msg") == "worker pass completed" {
				return true
			}
		}

		return false
	})
	unchanged := readLotState(t, ctx, pool, lotID)
	assert.Equal(t, lot.StatusFinished, unchanged.status)
	require.NotNil(t, unchanged.finishedAt)
	assert.True(t, finished.finishedAt.Equal(*unchanged.finishedAt), "finished_at must not change")
	require.NotNil(t, unchanged.winningBidID)
	assert.Equal(t, winnerID, *unchanged.winningBidID, "the winner must not change")

	crashed.assertLogsAreClean(t)
	successor.assertLogsAreClean(t)
	afterFinish.assertLogsAreClean(t)
}

// TestGracefulShutdownDrainsHTTPAndWorker sends SIGTERM while the background
// transaction sits blocked inside the finishing UPDATE: the process stops
// accepting new requests, rolls the background transaction back within the
// shutdown budget and exits cleanly — leaving the lot to the next replica.
//
// The scenario needs a deliverable SIGTERM and therefore runs on Linux (CI);
// the shutdown mechanics themselves are unit-tested in internal/app.
func TestGracefulShutdownDrainsHTTPAndWorker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("SIGTERM cannot be delivered to a process on %s; the scenario runs on Linux CI", runtime.GOOS)
	}

	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Корректная остановка")
	participantID := createUser(t, ctx, pool, "mp-sigterm-participant", "participant", "multiproc-participant")
	lotID := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Second))
	winnerID := insertBidRow(t, ctx, pool, lotID, participantID, 150, time.Now().Add(-2*time.Minute),
		"16000000-0000-4000-8000-000000000001")

	barrier := holdBidRow(t, pool, winnerID)
	stopping := startReplica(t, "stopping", nil)
	waitFor(t, 15*time.Second, "the worker pass to block on the barrier", func() bool {
		return waitingLockCount(t, pool) > 0
	})

	stopping.send(t, syscall.SIGTERM)

	// The listener stops taking new requests as soon as the shutdown starts.
	waitFor(t, 3*time.Second, "the listener to stop accepting requests", func() bool {
		req, err := http.NewRequestWithContext(backgroundContext(), http.MethodGet, stopping.base+"/livez", nil)
		if err != nil {
			return true
		}
		resp, err := (&http.Client{Timeout: time.Second}).Do(req) //nolint:bodyclose // the body is closed immediately; a refused connection is the expected outcome
		if err != nil {
			return true
		}
		_ = resp.Body.Close()

		return false
	})

	// One budget covers HTTP and the background loop: the process exits
	// cleanly, the abandoned transaction rolled back.
	waitFor(t, 15*time.Second, "the process to exit within the shutdown budget", stopping.isDone)
	assert.Equal(t, 0, stopping.exited(t), "a graceful shutdown exits with code 0")

	state := readLotState(t, ctx, pool, lotID)
	assert.Equal(t, lot.StatusActive, state.status, "the interrupted pass must not leave a partial result")
	assert.Nil(t, state.winningBidID)
	waitFor(t, 10*time.Second, "PostgreSQL to release the stopped process's locks", func() bool {
		return lotRowUnlocked(t, pool, lotID)
	})

	// The released lot goes to the next replica.
	barrier.release()
	successor := startReplica(t, "sigterm-successor", nil)
	finished := waitLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, finished.winningBidID)
	assert.Equal(t, winnerID, *finished.winningBidID)

	stopping.assertLogsAreClean(t)
	successor.assertLogsAreClean(t)
}
