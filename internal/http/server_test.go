package httpapp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLivezAndRequestLog(t *testing.T) {
	var logs bytes.Buffer
	h, err := NewHandler(slog.New(slog.NewJSONHandler(&logs, nil)), &fakeAuthenticator{}, &fakeCategories{}, &fakeLots{}, testConfig())
	require.NoError(t, err)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil))
	assert.Equal(t, http.StatusOK, r.Code)
	assert.Equal(t, "ok\n", r.Body.String())
	id := r.Header().Get("X-Request-ID")
	assert.Len(t, id, 32)
	var event map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event))
	assert.Equal(t, id, event["request_id"])
	assert.Equal(t, float64(http.StatusOK), event["status"])
}

func TestAPIErrorShape(t *testing.T) {
	h, err := NewHandler(discardLogger(), &fakeAuthenticator{}, &fakeCategories{}, &fakeLots{}, testConfig())
	require.NoError(t, err)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/missing", nil))
	assert.Equal(t, http.StatusNotFound, r.Code)
	assert.Contains(t, r.Header().Get("Content-Type"), "application/json")
	var response errorResponse
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &response))
	assert.Equal(t, "not_found", response.Error.Code)
	assert.Equal(t, r.Header().Get("X-Request-ID"), response.Error.RequestID)
}

func TestPanicBecomesInternalError(t *testing.T) {
	var logs bytes.Buffer
	h := &Handler{
		logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		pages:  mustParsePages(t),
	}
	wrapped := h.enrichWithID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("sensitive value") }))
	r := httptest.NewRecorder()
	wrapped.ServeHTTP(r, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/test", nil))
	assert.Equal(t, http.StatusInternalServerError, r.Code)
	assert.NotContains(t, r.Body.String(), "sensitive value")
	assert.NotContains(t, logs.String(), "sensitive value")
}
