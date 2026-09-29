package observability

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go" // package dto, directory named go
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPRequestMetricsAggregateByRouteTemplate(t *testing.T) {
	m := NewMetrics(nil)

	m.ObserveHTTPRequest("GET", "/lots/{id}", http.StatusOK, 12*time.Millisecond)
	m.ObserveHTTPRequest("GET", "/lots/{id}", http.StatusOK, 20*time.Millisecond)
	m.ObserveHTTPRequest("GET", "/lots/{id}", http.StatusNotFound, 3*time.Millisecond)

	require.Equal(t, 2.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/lots/{id}", "200")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/lots/{id}", "404")))
	// The duration histogram carries method and route only: statuses stay on
	// the counter, so every route keeps exactly one histogram series.
	require.Equal(t, 1, testutil.CollectAndCount(m.httpDuration, "auction_http_request_duration_seconds"))
}

func TestBidAttemptMetricsCountEachOutcome(t *testing.T) {
	m := NewMetrics(nil)

	m.CountBidAttempt(BidOutcomeAccepted, BidReasonNone)
	m.CountBidAttempt(BidOutcomeReplayed, BidReasonNone)
	m.CountBidAttempt(BidOutcomeRejected, "bid_too_low")
	m.CountBidAttempt(BidOutcomeTechnical, "internal")

	require.Equal(t, 1.0, testutil.ToFloat64(m.bidAttempts.WithLabelValues(BidOutcomeAccepted, BidReasonNone)))
	require.Equal(t, 1.0, testutil.ToFloat64(m.bidAttempts.WithLabelValues(BidOutcomeReplayed, BidReasonNone)))
	require.Equal(t, 1.0, testutil.ToFloat64(m.bidAttempts.WithLabelValues(BidOutcomeRejected, "bid_too_low")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.bidAttempts.WithLabelValues(BidOutcomeTechnical, "internal")))
}

func TestBidTransactionDurationObserved(t *testing.T) {
	m := NewMetrics(nil)

	m.ObserveBidTransaction(250 * time.Millisecond)

	assert.Equal(t, 1, testutil.CollectAndCount(m.bidDuration, "auction_bid_transaction_duration_seconds"))
}

func TestAuctionFinishMetricsCountWinnerAndDelay(t *testing.T) {
	m := NewMetrics(nil)

	m.AuctionFinished(true, 1500*time.Millisecond)
	m.AuctionFinished(false, 3*time.Second)

	require.Equal(t, 1.0, testutil.ToFloat64(m.auctions.WithLabelValues("with_winner")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.auctions.WithLabelValues("without_winner")))
	metrics, err := m.registry.Gather()
	require.NoError(t, err)
	finishCount, found := gatherHistogramCount(metrics, "auction_auction_finish_delay_seconds")
	require.True(t, found, "the finish delay histogram must be gathered")
	assert.Equal(t, uint64(2), finishCount)
}

func TestWorkerGaugeStaysZeroUntilFirstSuccess(t *testing.T) {
	m := NewMetrics(nil)

	require.Zero(t, testutil.ToFloat64(m.workerSuccess), "the gauge is zero before the first successful pass")

	passed := time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)
	m.WorkerPassSucceeded(passed)
	assert.Equal(t, float64(passed.Unix()), testutil.ToFloat64(m.workerSuccess))

	later := passed.Add(time.Minute)
	m.WorkerPassSucceeded(later)
	assert.Equal(t, float64(later.Unix()), testutil.ToFloat64(m.workerSuccess))
}

func TestPoolSnapshotCollectorExposesPoolState(t *testing.T) {
	snapshot := PoolSnapshot{
		TotalConns:           7,
		AcquiredConns:        3,
		IdleConns:            4,
		MaxConns:             10,
		EmptyAcquireCount:    12,
		CanceledAcquireCount: 1,
		EmptyAcquireWaitTime: 1500 * time.Millisecond,
	}
	m := NewMetrics(func() PoolSnapshot { return snapshot })

	for name, expected := range map[string]float64{
		"auction_db_pool_total_conns":                7,
		"auction_db_pool_acquired_conns":             3,
		"auction_db_pool_idle_conns":                 4,
		"auction_db_pool_max_conns":                  10,
		"auction_db_pool_acquire_wait_count_total":   12,
		"auction_db_pool_canceled_acquires_total":    1,
		"auction_db_pool_acquire_wait_seconds_total": 1.5,
	} {
		t.Run(name, func(t *testing.T) {
			metrics, err := m.registry.Gather()
			require.NoError(t, err)

			value, found := gatherMetricValue(metrics, name)
			require.True(t, found, "metric %s must be gathered", name)
			assert.InDelta(t, expected, value, 0.000001)
		})
	}
}

func TestRegistryCarriesGoProcessAndAuctionMetrics(t *testing.T) {
	m := NewMetrics(nil)
	m.ObserveHTTPRequest("GET", "/", http.StatusOK, time.Millisecond)

	metrics, err := m.registry.Gather()
	require.NoError(t, err)
	names := gatherMetricNames(metrics)
	for _, name := range []string{"go_goroutines", "auction_http_requests_total", "auction_worker_last_success_timestamp_seconds"} {
		assert.Contains(t, names, name)
	}
}

func TestMetricsHandlerServesPrometheusFormat(t *testing.T) {
	m := NewMetrics(nil)
	m.CountBidAttempt(BidOutcomeAccepted, BidReasonNone)

	server := httptest.NewServer(m.Handler())
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL) //nolint:noctx // a fixed local test target
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	assert.Contains(t, body.String(), "auction_bid_attempts_total")
}

// gatherMetricNames flattens the gathered families into their names.
func gatherMetricNames(families []*dto.MetricFamily) []string {
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}

	return names
}

// gatherMetricValue returns the value of the first metric of the named family.
func gatherMetricValue(families []*dto.MetricFamily, name string) (float64, bool) {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		inner := family.GetMetric()
		if len(inner) == 0 {
			return 0, false
		}
		switch {
		case inner[0].GetCounter() != nil:
			return inner[0].GetCounter().GetValue(), true
		case inner[0].GetGauge() != nil:
			return inner[0].GetGauge().GetValue(), true
		default:
			return 0, false
		}
	}

	return 0, false
}

// gatherHistogramCount returns the observation count of the named histogram.
func gatherHistogramCount(families []*dto.MetricFamily, name string) (uint64, bool) {
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		inner := family.GetMetric()
		if len(inner) == 0 || inner[0].GetHistogram() == nil {
			return 0, false
		}

		return inner[0].GetHistogram().GetSampleCount(), true
	}

	return 0, false
}

func ExampleNewMetrics() {
	m := NewMetrics(nil)
	m.ObserveHTTPRequest("GET", "/lots/{id}", http.StatusOK, 12*time.Millisecond)
	fmt.Println("observed")
	// Output: observed
}
