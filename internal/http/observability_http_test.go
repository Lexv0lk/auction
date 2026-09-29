package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/observability"
)

var errProbeDatabaseDown = errors.New("read schema version: connection refused")

// observableConfig enables the metrics endpoint on top of the standard test
// configuration.
func observableConfig() Config {
	config := testConfig()
	config.MetricsEnabled = true

	return config
}

// metricValue gathers the registry and returns the counter or gauge value of
// the series with exactly the wanted labels.
func metricValue(t *testing.T, registry *prometheus.Registry, family string, want map[string]string) (float64, bool) {
	t.Helper()

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != len(want) {
				continue
			}
			matches := true
			for _, l := range labels {
				if value, found := want[l.GetName()]; !found || value != l.GetValue() {
					matches = false

					break
				}
			}
			if !matches {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			}
		}
	}

	return 0, false
}

func TestServiceProbesAnswerWithoutSession(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	server, guest := newTestServer(t, &fakeAuthenticator{}, observableConfig(), metrics)

	resp := getRequest(t, guest, server.URL+"/livez")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok\n", resp.Body)

	resp = getRequest(t, guest, server.URL+"/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode, "no readiness dependency configured: ready")
	assert.Equal(t, "ready\n", resp.Body)

	resp = getRequest(t, guest, server.URL+"/metrics")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	assert.Contains(t, resp.Body, "# HELP", "the answer is the Prometheus exposition format")
}

func TestMetricsEndpointGatedByEnvironment(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	server, guest := newTestServer(t, &fakeAuthenticator{}, testConfig(), metrics)

	resp := getRequest(t, guest, server.URL+"/metrics")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "METRICS_ENABLED=false keeps the exporter off")
}

func TestReadyzReflectsDatabaseState(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	config := observableConfig()
	config.Readiness = func(context.Context) error { return nil }
	server, guest := newTestServer(t, &fakeAuthenticator{}, config, metrics)

	resp := getRequest(t, guest, server.URL+"/readyz")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	config.Readiness = func(context.Context) error { return errProbeDatabaseDown }
	server, guest = newTestServer(t, &fakeAuthenticator{}, config, metrics)

	resp = getRequest(t, guest, server.URL+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "not ready\n", resp.Body)
	assert.NotContains(t, resp.Body, "connection refused", "the diagnosis never reaches the answer body")
}

func TestReadyzStopsBeingReadyDuringShutdown(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	config := observableConfig()
	readinessCalls := 0
	config.Readiness = func(context.Context) error {
		readinessCalls++

		return nil
	}
	config.Draining = func() bool { return true }
	server, guest := newTestServer(t, &fakeAuthenticator{}, config, metrics)

	resp := getRequest(t, guest, server.URL+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Zero(t, readinessCalls, "a draining process answers without a database check")

	resp = getRequest(t, guest, server.URL+"/livez")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "liveness stays independent of the drain")
}

func TestHTTPMetricsKeepRouteTemplateLabel(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	lots := &fakeLots{publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
		return activeBidLot(), nil
	}}
	server, client := loggedInClient(t, auth.User{ID: 2, Role: auth.RoleParticipant}, lots, metrics)

	for _, target := range []string{"/lots/5", "/lots/6", "/lots/999"} {
		resp := getRequest(t, client, server.URL+target)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	value, found := metricValue(t, metrics.Registry(), "auction_http_requests_total",
		map[string]string{"method": "GET", "route": "/lots/{id}", "status": "200"})
	require.True(t, found, "the route series must exist")
	assert.Equal(t, 3.0, value, "different lot IDs share one route series")

	_, found = metricValue(t, metrics.Registry(), "auction_http_requests_total",
		map[string]string{"method": "GET", "route": "/lots/5", "status": "200"})
	assert.False(t, found, "a concrete lot ID must never become a route label")

	resp := getRequest(t, client, server.URL+"/totally-unknown")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_, found = metricValue(t, metrics.Registry(), "auction_http_requests_total",
		map[string]string{"method": "GET", "route": "unmatched", "status": "404"})
	assert.True(t, found, "unknown routes answer under the fixed unmatched label")
}

func TestBidAttemptMetricsDistinguishOutcomes(t *testing.T) {
	participant := auth.User{ID: 2, Login: "demo-participant-1", Role: auth.RoleParticipant}

	tests := map[string]struct {
		placeFunc      func() (lot.PlacedBid, error)
		payload        string
		expectedStatus int
		expected       map[string]string
	}{
		"accepted": {
			placeFunc:      func() (lot.PlacedBid, error) { return placedBid(false), nil },
			expectedStatus: http.StatusCreated,
			expected:       map[string]string{"outcome": "accepted", "reason": "none"},
		},
		"replayed": {
			placeFunc:      func() (lot.PlacedBid, error) { return placedBid(true), nil },
			expectedStatus: http.StatusOK,
			expected:       map[string]string{"outcome": "replayed", "reason": "none"},
		},
		"rejected as too low": {
			placeFunc:      func() (lot.PlacedBid, error) { return lot.PlacedBid{}, lot.ErrBidTooLow },
			expectedStatus: http.StatusConflict,
			expected:       map[string]string{"outcome": "rejected", "reason": "bid_too_low"},
		},
		"technical failure": {
			placeFunc:      func() (lot.PlacedBid, error) { return lot.PlacedBid{}, errProbeDatabaseDown },
			expectedStatus: http.StatusServiceUnavailable,
			expected:       map[string]string{"outcome": "technical_error", "reason": "internal"},
		},
		"unknown commit outcome": {
			placeFunc:      func() (lot.PlacedBid, error) { return lot.PlacedBid{}, commitLostError() },
			expectedStatus: http.StatusServiceUnavailable,
			expected:       map[string]string{"outcome": "technical_error", "reason": "commit_outcome_unknown"},
		},
		"invalid amount never reaches the transaction": {
			placeFunc:      func() (lot.PlacedBid, error) { return placedBid(false), nil },
			payload:        bidAPIBody("0", validBidKey),
			expectedStatus: http.StatusUnprocessableEntity,
			expected:       map[string]string{"outcome": "rejected", "reason": "invalid_bid"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			metrics := observability.NewMetrics(nil)
			lots := &fakeLots{placeFunc: func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) {
				return tc.placeFunc()
			}}
			server, client := loggedInClient(t, participant, lots, metrics)
			token := bidAPIToken(t, client, server.URL)

			payload := tc.payload
			if payload == "" {
				payload = bidAPIBody("101", validBidKey)
			}
			resp := postJSON(t, client, server.URL+"/api/lots/7/bids", payload,
				map[string]string{"X-CSRF-Token": token})
			require.Equal(t, tc.expectedStatus, resp.StatusCode)

			value, found := metricValue(t, metrics.Registry(), "auction_bid_attempts_total", tc.expected)
			require.True(t, found, "the outcome series %v must exist", tc.expected)
			assert.Equal(t, 1.0, value)

			// The CSRF middleware wraps the response writer on POST requests:
			// the route label must survive that wrapping.
			_, found = metricValue(t, metrics.Registry(), "auction_http_requests_total",
				map[string]string{"method": "POST", "route": "/api/lots/{id}/bids", "status": strconv.Itoa(tc.expectedStatus)})
			require.True(t, found, "the POST route template must be recorded")
		})
	}
}

func TestBidFormReplayIsNotCountedAsAccepted(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	lots := &fakeLots{
		publicFunc: func(context.Context, int64) (lot.PublicLot, error) { return activeBidLot(), nil },
		bidsFunc:   func(context.Context, int64, int) ([]lot.Bid, bool, error) { return nil, false, nil },
		placeFunc:  func(context.Context, int64, int64, int64, string) (lot.PlacedBid, error) { return placedBid(true), nil },
	}
	server, client := loggedInClient(t, auth.User{ID: 2, Role: auth.RoleParticipant}, lots, metrics)
	token := pageCSRFToken(t, client, server.URL+"/lots/7")

	resp := postForm(t, client, server.URL+"/lots/7/bids", url.Values{
		"amount":             {"101"},
		"request_key":        {validBidKey},
		"gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	_, found := metricValue(t, metrics.Registry(), "auction_bid_attempts_total",
		map[string]string{"outcome": "accepted", "reason": "none"})
	assert.False(t, found, "a replay is not a newly accepted bid")
	replayed, found := metricValue(t, metrics.Registry(), "auction_bid_attempts_total",
		map[string]string{"outcome": "replayed", "reason": "none"})
	require.True(t, found)
	assert.Equal(t, 1.0, replayed)
}

func TestRequestLogCarriesOperationOutcomeAndErrorCode(t *testing.T) {
	var logBuf strings.Builder
	config := observableConfig()
	config.Readiness = func(context.Context) error { return errProbeDatabaseDown }
	metrics := observability.NewMetrics(nil)
	handler, err := NewHandler(slog.New(slog.NewJSONHandler(&logBuf, nil)), &fakeAuthenticator{}, &fakeCategories{}, &fakeLots{}, metrics, config)
	require.NoError(t, err)
	server := startTestServer(t, handler)
	// A probe-like client: no cookie jar, no session, no CSRF state.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp := getRequest(t, client, server.URL+"/api/lots/7")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a guest is refused by the session gate")

	resp = getRequest(t, client, server.URL+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var requestLog, readinessLog map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		switch {
		case event["msg"] == "http request" && event["operation"] == "/api/lots/{id}":
			requestLog = event
		case event["msg"] == "readiness check failed":
			readinessLog = event
		}
	}
	require.NotNil(t, requestLog, "the request event must be logged")
	assert.Equal(t, "refused", requestLog["outcome"])
	assert.Equal(t, float64(http.StatusUnauthorized), requestLog["status"])
	assert.Equal(t, "unauthenticated", requestLog["error_code"])
	assert.Contains(t, requestLog, "duration", "the log carries the response duration")

	require.NotNil(t, readinessLog, "the failed readiness check is diagnosed in the log")
	assert.Equal(t, "not_ready", readinessLog["error_code"])
	assert.Contains(t, readinessLog["error"], "connection refused")
}

func TestRequestLogOutcomeMarksSuccess(t *testing.T) {
	var logBuf strings.Builder
	metrics := observability.NewMetrics(nil)
	lots := &fakeLots{publicFunc: func(context.Context, int64) (lot.PublicLot, error) {
		return activeBidLot(), nil
	}}
	sessionToken := strings.Repeat("f", 64)
	authenticator := &fakeAuthenticator{
		userFunc: func(_ context.Context, token string) (auth.User, error) {
			if token == sessionToken {
				return auth.User{ID: 2, Role: auth.RoleParticipant}, nil
			}

			return auth.User{}, auth.ErrNoSession
		},
	}
	handler, err := NewHandler(slog.New(slog.NewJSONHandler(&logBuf, nil)), authenticator, &fakeCategories{}, lots, metrics, observableConfig())
	require.NoError(t, err)
	server := startTestServer(t, handler)
	jar, jarErr := cookiejar.New(nil)
	require.NoError(t, jarErr)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	setSessionCookie(t, client, server.URL, sessionToken)

	resp := getRequest(t, client, server.URL+"/lots/7")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var candidate map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &candidate))
		if candidate["msg"] == "http request" && candidate["operation"] == "/lots/{id}" {
			event = candidate
		}
	}
	require.NotNil(t, event)
	assert.Equal(t, "ok", event["outcome"])
	_, hasErrorCode := event["error_code"]
	assert.False(t, hasErrorCode, "a successful answer carries no error code")
}

// startTestServer exposes a handler behind an httptest server.
func startTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}
