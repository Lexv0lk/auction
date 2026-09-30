//go:build integration

package multiproc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/testutil"
)

var lotEditPathPattern = regexp.MustCompile(`/admin/lots/(\d+)/edit`)

// TestEndToEndAcrossReplicas walks the whole auction through two real server
// processes: the administrator creates and publishes through the first
// replica, the participants bid through both replicas with nothing but their
// cookies and CSRF tokens, the background loop of one of the replicas records
// the result and the page shows the winner. The repeated request key of a
// committed bid answers as a replay — the lost-response case.
func TestEndToEndAcrossReplicas(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	adminPass, participantPass := "multiproc-admin", "multiproc-participant"
	createUser(t, ctx, pool, "mp-admin", "admin", adminPass)
	createUser(t, ctx, pool, "mp-participant-1", "participant", participantPass)
	createUser(t, ctx, pool, "mp-participant-2", "participant", participantPass)

	first := startReplica(t, "replica-1", nil)
	second := startReplica(t, "replica-2", nil)

	// The administrator works through the first replica: category, lot,
	// publication — every step through the real forms with CSRF tokens.
	admin := newClient(t)
	login(t, admin, first.base, "mp-admin", adminPass)

	categoryToken := pageToken(t, admin, first.base+"/admin/categories")
	resp := postForm(t, admin, first.base+"/admin/categories", url.Values{
		"name":               {"Многопроцессная категория"},
		"gorilla.csrf.Token": {categoryToken},
	})
	require.Equal(t, http.StatusSeeOther, resp.Status, "the category must be created: %s", resp.Body)
	var categoryID int64
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT id FROM categories WHERE name = 'Многопроцессная категория'").Scan(&categoryID))

	lotToken := pageToken(t, admin, first.base+"/admin/lots/new")
	resp = postForm(t, admin, first.base+"/admin/lots", url.Values{
		"title":              {"Многопроцессный лот"},
		"description":        {"Сквозной сценарий двух реплик"},
		"category_id":        {strconv.FormatInt(categoryID, 10)},
		"start_price":        {"100"},
		"ends_at":            {formatLotDeadline(time.Now().Add(2 * time.Minute))},
		"ends_at_tz":         {"UTC"},
		"gorilla.csrf.Token": {lotToken},
	})
	require.Equal(t, http.StatusSeeOther, resp.Status, "the lot must be created: %s", resp.Body)
	editMatch := lotEditPathPattern.FindStringSubmatch(resp.Header.Get("Location"))
	require.NotNil(t, editMatch, "the creation must redirect to the edit page")
	lotID := parseInt64(t, editMatch[1])

	resp = postForm(t, admin, first.base+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/publish",
		url.Values{"gorilla.csrf.Token": {lotToken}})
	require.Equal(t, http.StatusSeeOther, resp.Status, "the lot must be published: %s", resp.Body)
	require.Equal(t, lot.StatusActive, readLotState(t, ctx, pool, lotID).status)

	// The participants enter through different replicas: the sessions live
	// in PostgreSQL, so both replicas accept the same cookies.
	firstParticipant := newClient(t)
	login(t, firstParticipant, second.base, "mp-participant-1", participantPass)
	secondParticipant := newClient(t)
	login(t, secondParticipant, first.base, "mp-participant-2", participantPass)
	firstToken := pageToken(t, firstParticipant, second.base+"/login")
	secondToken := pageToken(t, secondParticipant, first.base+"/login")

	lotPath := strconv.FormatInt(lotID, 10)

	// The first participant bids through the second replica; the answer
	// carries the stored bid.
	resp = postJSON(t, firstParticipant, second.base+"/api/lots/"+lotPath+"/bids",
		bidAPIBody("100", "11000000-0000-4000-8000-000000000001"),
		map[string]string{"X-CSRF-Token": firstToken})
	require.Equal(t, http.StatusCreated, resp.Status, "the first bid must be accepted: %s", resp.Body)
	firstBidID := parseInt64(t, bidIDFromAPIResponse(t, resp.Body))

	// The second participant bids through the first replica via the plain
	// form: the key comes from the rendered page of that replica.
	secondKey := pageBidKey(t, secondParticipant, first.base+"/lots/"+lotPath)
	resp = postForm(t, secondParticipant, first.base+"/lots/"+lotPath+"/bids", url.Values{
		"amount":             {"101"},
		"request_key":        {secondKey},
		"gorilla.csrf.Token": {pageToken(t, secondParticipant, first.base+"/login")},
	})
	require.Equal(t, http.StatusSeeOther, resp.Status, "the form bid must be accepted")
	require.Contains(t, resp.Header.Get("Location"), "?placed=")

	// A lost answer after COMMIT: the same request key returns the stored
	// bid and writes nothing.
	resp = postJSON(t, firstParticipant, second.base+"/api/lots/"+lotPath+"/bids",
		bidAPIBody("100", "11000000-0000-4000-8000-000000000001"),
		map[string]string{"X-CSRF-Token": firstToken})
	require.Equal(t, http.StatusOK, resp.Status)
	var replay struct {
		Bid struct {
			ID string `json:"id"`
		} `json:"bid"`
		Replayed bool `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &replay))
	assert.True(t, replay.Replayed)
	assert.Equal(t, strconv.FormatInt(firstBidID, 10), replay.Bid.ID)
	assert.Equal(t, int64(2), countBids(t, ctx, pool, lotID))

	// Two participants race for the same amount through different replicas:
	// exactly one bid of 110 is accepted.
	raceKeys := [2]string{"11000000-0000-4000-8000-000000000002", "11000000-0000-4000-8000-000000000003"}
	raceTargets := [2]string{
		second.base + "/api/lots/" + lotPath + "/bids",
		first.base + "/api/lots/" + lotPath + "/bids",
	}
	raceClients := [2]*http.Client{firstParticipant, secondParticipant}
	raceTokens := [2]string{firstToken, secondToken}
	type raceResult struct {
		page page
		err  error
	}
	results := make([]raceResult, 2)
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index].page, results[index].err = tryPostJSON(raceClients[index], raceTargets[index],
				bidAPIBody("110", raceKeys[index]), map[string]string{"X-CSRF-Token": raceTokens[index]})
		}(i)
	}
	close(start)
	wg.Wait()

	winnerID := int64(0)
	for index, result := range results {
		require.NoError(t, result.err, "race attempt %d must be answered", index+1)
		switch result.page.Status {
		case http.StatusCreated:
			require.Zero(t, winnerID, "only one race bid may be accepted")
			winnerID = parseInt64(t, bidIDFromAPIResponse(t, result.page.Body))
		case http.StatusConflict:
			assert.Contains(t, result.page.Body, "bid_too_low")
		default:
			require.Failf(t, "unexpected race answer", "attempt %d answered %d: %s",
				index+1, result.page.Status, result.page.Body)
		}
	}
	require.NotZero(t, winnerID, "the maximal amount is always accepted")
	assert.Equal(t, int64(3), countBids(t, ctx, pool, lotID))
	assert.Equal(t, int64(110), maxBidAmount(t, ctx, pool, lotID))

	// The deadline decides on the database clock: the late bid is refused
	// while the workers have not run yet.
	setLotDeadline(t, ctx, pool, lotID, time.Now().Add(-time.Second))
	resp = postJSON(t, firstParticipant, second.base+"/api/lots/"+lotPath+"/bids",
		bidAPIBody("150", "11000000-0000-4000-8000-000000000004"),
		map[string]string{"X-CSRF-Token": firstToken})
	require.Equal(t, http.StatusConflict, resp.Status)
	assert.Contains(t, resp.Body, "auction_closed")
	assert.Equal(t, int64(3), countBids(t, ctx, pool, lotID))

	// One of the two background loops records the result; the page of the
	// other replica shows the server-recorded winner.
	state := waitLotFinished(t, ctx, pool, lotID)
	require.NotNil(t, state.winningBidID)
	assert.Equal(t, winnerID, *state.winningBidID)

	winnerPage := get(t, firstParticipant, second.base+"/lots/"+lotPath)
	require.Equal(t, http.StatusOK, winnerPage.Status)
	assert.Contains(t, winnerPage.Body, "Победитель")
	assert.Contains(t, winnerPage.Body, ">110<")

	// Both processes served the scenario: the logs stay clean JSON without
	// secrets.
	first.assertLogsAreClean(t)
	second.assertLogsAreClean(t)
}

// TestTwoReplicasFinishLotsExactlyOnce starts two server processes whose
// background loops compete for a set of due lots: every lot is finished
// exactly once across both processes, untouched lots stay untouched, and a
// restart of both processes changes nothing.
func TestTwoReplicasFinishLotsExactlyOnce(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	categoryID := createCategoryRow(t, ctx, pool, "Многопроцессное завершение")
	participant := createUser(t, ctx, pool, "mp-finish-participant", "participant", "multiproc-participant")

	const dueLots = 7
	lotWinners := make(map[int64]int64, dueLots)
	bidlessLot := int64(0)
	for i := 0; i < dueLots; i++ {
		lotID := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Second))
		if i < dueLots-1 {
			bidID := insertBidRow(t, ctx, pool, lotID, participant, int64(100+10*i),
				time.Now().Add(-2*time.Minute), "12000000-0000-4000-8000-"+bidKeySuffix(i))
			lotWinners[lotID] = bidID
		} else {
			// The last due lot stays bidless: it must finish with a NULL winner.
			bidlessLot = lotID
		}
	}
	futureLot := createLotRow(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(time.Hour))
	insertBidRow(t, ctx, pool, futureLot, participant, 150, time.Now().Add(-time.Minute),
		"12000000-0000-4000-8000-000000000099")
	draftLot := createLotRow(t, ctx, pool, categoryID, lot.StatusDraft, 100, time.Now().Add(-time.Second))

	first := startReplica(t, "replica-1", nil)
	second := startReplica(t, "replica-2", nil)

	// Every due lot is finished once; the stored winner is the maximal
	// accepted bid.
	finishedBefore := make(map[int64]lotRowState, dueLots)
	for lotID, bidID := range lotWinners {
		state := waitLotFinished(t, ctx, pool, lotID)
		require.NotNil(t, state.winningBidID)
		assert.Equal(t, bidID, *state.winningBidID)
		finishedBefore[lotID] = state
	}
	bidlessState := waitLotFinished(t, ctx, pool, bidlessLot)
	assert.Nil(t, bidlessState.winningBidID, "a lot without bids keeps the NULL winner")

	futureState := readLotState(t, ctx, pool, futureLot)
	assert.Equal(t, lot.StatusActive, futureState.status, "a future lot is not due")
	assert.Nil(t, futureState.finishedAt)
	assert.Equal(t, lot.StatusDraft, readLotState(t, ctx, pool, draftLot).status)

	// The committed completions of both processes cover every due lot
	// exactly once: the lots a pass found locked were picked up by the
	// passes of the other replica (FOR UPDATE SKIP LOCKED).
	committed := map[int64]int{}
	for _, replica := range []*replica{first, second} {
		for lotID, count := range replica.finishedLotIDs(t) {
			committed[lotID] += count
		}
	}
	for lotID := range lotWinners {
		assert.Equal(t, 1, committed[lotID], "lot %d must be finished exactly once", lotID)
	}
	assert.Equal(t, 1, committed[bidlessLot])
	assert.Len(t, committed, dueLots, "no other lots were finished")

	// A restart of both processes changes nothing: finished lots are never
	// selected again, and the new processes record no completions.
	first.kill()
	second.kill()
	restartedFirst := startReplica(t, "restart-1", nil)
	restartedSecond := startReplica(t, "restart-2", nil)
	for _, replica := range []*replica{restartedFirst, restartedSecond} {
		waitFor(t, 10*time.Second, "the worker pass of the restarted process", func() bool {
			for _, line := range replica.logs.lines() {
				if logField(line, "msg") == "worker pass completed" {
					return true
				}
			}

			return false
		})
	}
	for lotID, before := range finishedBefore {
		after := readLotState(t, ctx, pool, lotID)
		assert.Equal(t, lot.StatusFinished, after.status)
		require.NotNil(t, after.finishedAt)
		assert.True(t, before.finishedAt.Equal(*after.finishedAt), "finished_at of lot %d must not change", lotID)
		require.NotNil(t, after.winningBidID)
		assert.Equal(t, *before.winningBidID, *after.winningBidID, "the winner of lot %d must not change", lotID)
	}
	for _, replica := range []*replica{restartedFirst, restartedSecond} {
		assert.Empty(t, replica.finishedLotIDs(t), "a restart must not re-finish anything")
		replica.assertLogsAreClean(t)
	}
}

// tryPostJSON posts without assertions: concurrent attempts collect their
// outcomes and assert them on the test goroutine (require never runs on a
// child goroutine).
func tryPostJSON(client *http.Client, target, payload string, headers map[string]string) (page, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(payload))
	if err != nil {
		return page{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return page{}, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()

		return page{}, err
	}
	if err = resp.Body.Close(); err != nil {
		return page{}, err
	}

	return page{Status: resp.StatusCode, Header: resp.Header, Body: string(body)}, nil
}

// bidIDFromAPIResponse extracts the stored bid id of an accepted or replayed
// API answer.
func bidIDFromAPIResponse(t *testing.T, body string) string {
	t.Helper()

	var parsed struct {
		Bid struct {
			ID string `json:"id"`
		} `json:"bid"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))

	return parsed.Bid.ID
}

// logField returns the string value of one JSON log record field.
func logField(line, name string) string {
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		return ""
	}
	text, _ := record[name].(string)

	return text
}

// bidKeySuffix renders a unique request key tail for fixture bids.
func bidKeySuffix(i int) string {
	return fmt.Sprintf("%012d", i+1)
}
