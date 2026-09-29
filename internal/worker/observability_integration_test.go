//go:build integration

package worker

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/observability"
	"github.com/Lexv0lk/auction/internal/testutil"
)

// findFamilyValue gathers the registry and returns the counter or gauge value
// of the named family (single-series families only).
func findFamilyValue(t *testing.T, registry metricsRegistry, name string) (float64, bool) {
	t.Helper()

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name || len(family.GetMetric()) == 0 {
			continue
		}
		metric := family.GetMetric()[0]
		switch {
		case metric.GetCounter() != nil:
			return metric.GetCounter().GetValue(), true
		case metric.GetGauge() != nil:
			return metric.GetGauge().GetValue(), true
		}
	}

	return 0, false
}

type metricsRegistry interface {
	Gather() ([]*dto.MetricFamily, error)
}

func TestMetricsRecordCommittedCompletionsAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	metrics := observability.NewMetrics(nil)
	categoryID := createWorkerCategory(t, ctx, pool, "Нумизматика worker наблюдаемость")
	lotID := createWorkerLot(t, ctx, pool, categoryID, lot.StatusActive, 100, time.Now().Add(-time.Minute))
	participantID := createWorkerUser(t, ctx, pool, "metrics-participant")
	insertWorkerBid(t, ctx, pool, lotID, participantID, 150, time.Now().Add(-2*time.Minute))

	w := New(pool, discardLogger(), metrics)

	// One pass finishes the due lot; a second pass finds no work and must not
	// repeat the completion counter.
	for range 2 {
		finished, err := w.pass(ctx, 10)
		require.NoError(t, err)
		if finished == 0 {
			break
		}
	}

	withWinner, found := findFamilyValue(t, metrics.Registry(), "auction_auctions_finished_total")
	require.True(t, found, "the completion counter must exist after the committed finish")
	assert.Equal(t, 1.0, withWinner, "a repeated pass never counts the same lot again")

	// The success gauge belongs to the loop: a successful pass (even an empty
	// one) advances it, and a finished lot is not counted a second time.
	runCtx, cancelRun := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancelRun()
	require.NoError(t, w.Run(runCtx, config.Worker{PollInterval: 5 * time.Millisecond, BatchSize: 10}))
	success, found := findFamilyValue(t, metrics.Registry(), "auction_worker_last_success_timestamp_seconds")
	require.True(t, found)
	assert.Greater(t, success, 0.0, "a committed pass advances the success gauge")

	withWinner, found = findFamilyValue(t, metrics.Registry(), "auction_auctions_finished_total")
	require.True(t, found)
	assert.Equal(t, 1.0, withWinner, "the loop passes never repeat the completion")

	families, err := metrics.Registry().Gather()
	require.NoError(t, err)
	histogramFound := false
	for _, family := range families {
		if family.GetName() != "auction_auction_finish_delay_seconds" {
			continue
		}
		require.Len(t, family.GetMetric(), 1)
		assert.Equal(t, uint64(1), family.GetMetric()[0].GetHistogram().GetSampleCount(), "one committed delay is observed")
		histogramFound = true
	}
	assert.True(t, histogramFound, "the finish delay histogram must be exported")
}
