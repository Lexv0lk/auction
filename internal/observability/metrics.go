package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Bid attempt outcome and reason label values. The outcomes separate business
// refusals, replays and technical failures; the reasons form a fixed set, so
// the series cardinality stays bounded no matter what the clients send.
const (
	BidOutcomeAccepted  = "accepted"
	BidOutcomeReplayed  = "replayed"
	BidOutcomeRejected  = "rejected"
	BidOutcomeTechnical = "technical_error"

	// BidReasonNone marks the outcomes that carry no refusal reason.
	BidReasonNone = "none"
	// BidReasonInternal marks an unmapped technical failure.
	BidReasonInternal = "internal"
)

// Auction finish result label values.
const (
	FinishResultWithWinner    = "with_winner"
	FinishResultWithoutWinner = "without_winner"
)

// PoolSnapshot is a point-in-time reading of the shared database pool. The
// application converts the pgx pool statistics into this struct, so the
// metrics package stays free of the database driver and the unit tests can
// feed arbitrary snapshots.
type PoolSnapshot struct {
	TotalConns           int32
	AcquiredConns        int32
	IdleConns            int32
	MaxConns             int32
	EmptyAcquireCount    int64         // acquires that found the pool empty
	CanceledAcquireCount int64         // acquires given up while waiting
	EmptyAcquireWaitTime time.Duration // cumulative time acquires spent waiting for a free connection
}

// Metrics carries the Prometheus instruments of one server process: the HTTP
// layer, the bid operation, the background worker and the pool all record
// into the same registry, exported under the auction_ prefix (the standard
// Go and process metrics keep their usual go_ and process_ names). Counters
// live in process memory and reset on restart; PostgreSQL holds the exact
// history.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	bidAttempts   *prometheus.CounterVec
	bidDuration   prometheus.Histogram
	auctions      *prometheus.CounterVec
	finishDelay   prometheus.Histogram
	workerSuccess prometheus.Gauge
}

// NewMetrics builds the instrument set on a fresh registry. A nil
// poolSnapshot drops the pool collector: the exporter then answers with
// everything except the pool gauges.
func NewMetrics(poolSnapshot func() PoolSnapshot) *Metrics {
	registry := prometheus.NewRegistry()

	httpRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_http_requests_total",
		Help: "HTTP requests by route template, method and response status.",
	}, []string{"method", "route", "status"})

	// The web answers are short; the default buckets (5ms..10s) cover them.
	httpDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "auction_http_request_duration_seconds",
		Help:    "Duration of HTTP responses by route template and method. Percentiles are computed by the collection system from these buckets.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	bidAttempts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_bid_attempts_total",
		Help: "Bid attempts by outcome (accepted, replayed, rejected, technical_error) and a fixed set of refusal reasons.",
	}, []string{"outcome", "reason"})

	// A bid transaction includes the wait for the lot row lock, so the
	// buckets run from one millisecond up to about a minute.
	bidDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_bid_transaction_duration_seconds",
		Help:    "Duration of the bid operation, including the wait for the lot row lock. Percentiles are computed by the collection system from these buckets.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 9),
	})

	auctions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auction_auctions_finished_total",
		Help: "Committed auction completions by result, counted after the commit, never at candidate selection.",
	}, []string{"result"})

	finishDelay := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "auction_auction_finish_delay_seconds",
		Help:    "Delay between the lot deadline and the committed finish (finished_at - ends_at), observed after the commit. Percentiles are computed by the collection system from these buckets.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
	})

	workerSuccess := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "auction_worker_last_success_timestamp_seconds",
		Help: "Unix time of the last successful worker pass, including passes without work; 0 until the first success and not advanced on errors.",
	})

	registry.MustRegister(httpRequests, httpDuration, bidAttempts, bidDuration, auctions, finishDelay, workerSuccess,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if poolSnapshot != nil {
		registry.MustRegister(newPoolCollector(poolSnapshot))
	}

	return &Metrics{
		registry:      registry,
		httpRequests:  httpRequests,
		httpDuration:  httpDuration,
		bidAttempts:   bidAttempts,
		bidDuration:   bidDuration,
		auctions:      auctions,
		finishDelay:   finishDelay,
		workerSuccess: workerSuccess,
	}
}

// Handler serves the Prometheus exposition format from the private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry exposes the registry for tests and verification.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// ObserveHTTPRequest records one answered HTTP request. The route label is
// the template ("https://example.com/lots/{id}"), never the concrete path.
func (m *Metrics) ObserveHTTPRequest(method, route string, status int, duration time.Duration) {
	statusLabel := strconv.Itoa(status)
	m.httpRequests.WithLabelValues(method, route, statusLabel).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

// CountBidAttempt records one bid attempt outcome with its refusal reason.
func (m *Metrics) CountBidAttempt(outcome, reason string) {
	m.bidAttempts.WithLabelValues(outcome, reason).Inc()
}

// ObserveBidTransaction records one bid operation duration, including the
// wait for the row lock and refusals that spent time inside the transaction.
func (m *Metrics) ObserveBidTransaction(duration time.Duration) {
	m.bidDuration.Observe(duration.Seconds())
}

// AuctionFinished records one committed completion; the delay is the time
// between the deadline and the committed finish. The counter belongs after
// the commit: a selected-but-uncommitted candidate is not a completion.
func (m *Metrics) AuctionFinished(withWinner bool, delay time.Duration) {
	result := FinishResultWithoutWinner
	if withWinner {
		result = FinishResultWithWinner
	}
	m.auctions.WithLabelValues(result).Inc()
	m.finishDelay.Observe(delay.Seconds())
}

// WorkerPassSucceeded stamps the gauge with the time of a successful pass
// (an empty pass included). A failed pass never advances it.
func (m *Metrics) WorkerPassSucceeded(at time.Time) {
	m.workerSuccess.Set(float64(at.Unix()))
}

// poolCollector exports the pool snapshot as auction_db_pool_* metrics.
type poolCollector struct {
	snapshot func() PoolSnapshot

	totalConns    *prometheus.Desc
	acquiredConns *prometheus.Desc
	idleConns     *prometheus.Desc
	maxConns      *prometheus.Desc
	waitCount     *prometheus.Desc
	canceledCount *prometheus.Desc
	waitSeconds   *prometheus.Desc
}

func newPoolCollector(snapshot func() PoolSnapshot) poolCollector {
	return poolCollector{
		snapshot: snapshot,
		totalConns: prometheus.NewDesc("auction_db_pool_total_conns",
			"Currently open database connections.", nil, prometheus.Labels{}),
		acquiredConns: prometheus.NewDesc("auction_db_pool_acquired_conns",
			"Connections currently checked out by the application.", nil, prometheus.Labels{}),
		idleConns: prometheus.NewDesc("auction_db_pool_idle_conns",
			"Open connections currently idle in the pool.", nil, prometheus.Labels{}),
		maxConns: prometheus.NewDesc("auction_db_pool_max_conns",
			"Configured pool size limit.", nil, prometheus.Labels{}),
		waitCount: prometheus.NewDesc("auction_db_pool_acquire_wait_count_total",
			"Cumulative number of connection acquires that had to wait for a free connection.", nil, prometheus.Labels{}),
		canceledCount: prometheus.NewDesc("auction_db_pool_canceled_acquires_total",
			"Cumulative number of acquires given up while waiting for a connection.", nil, prometheus.Labels{}),
		waitSeconds: prometheus.NewDesc("auction_db_pool_acquire_wait_seconds_total",
			"Cumulative time acquires spent waiting for a free connection, in seconds.", nil, prometheus.Labels{}),
	}
}

func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalConns
	ch <- c.acquiredConns
	ch <- c.idleConns
	ch <- c.maxConns
	ch <- c.waitCount
	ch <- c.canceledCount
	ch <- c.waitSeconds
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.snapshot()
	ch <- prometheus.MustNewConstMetric(c.totalConns, prometheus.GaugeValue, float64(snapshot.TotalConns))
	ch <- prometheus.MustNewConstMetric(c.acquiredConns, prometheus.GaugeValue, float64(snapshot.AcquiredConns))
	ch <- prometheus.MustNewConstMetric(c.idleConns, prometheus.GaugeValue, float64(snapshot.IdleConns))
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(snapshot.MaxConns))
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(snapshot.EmptyAcquireCount))
	ch <- prometheus.MustNewConstMetric(c.canceledCount, prometheus.CounterValue, float64(snapshot.CanceledAcquireCount))
	ch <- prometheus.MustNewConstMetric(c.waitSeconds, prometheus.CounterValue, snapshot.EmptyAcquireWaitTime.Seconds())
}
