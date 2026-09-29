package httpapp

import (
	"context"
	"net/http"
	"time"
)

// responseWriter remembers the status of one request; the route template and
// the error code live in the route state of the request context.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}

	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(data)
}

// requestOutcome groups the status into the three log outcomes: the business
// refusals (4xx) and the technical errors (5xx) stay distinguishable.
func requestOutcome(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "refused"
	default:
		return "ok"
	}
}

func (h *Handler) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w}
		state := &routeState{route: "unmatched"}
		r = r.WithContext(context.WithValue(r.Context(), routeStateKey{}, state))
		start := time.Now()
		requestID, _ := TryGetRequestID(r.Context())

		defer func() {
			if value := recover(); value != nil {
				state.setErrorCode("internal_error")
				h.logger.Error("request panic", "operation", state.route, "outcome", "error", "error_code", "internal_error", "request_id", requestID)
				if rw.status == 0 {
					h.Error(rw, r, http.StatusInternalServerError, "internal_error", "Внутренняя ошибка сервера")
				}
			}

			if rw.status == 0 {
				rw.status = http.StatusOK
			}
			duration := time.Since(start)
			h.metrics.ObserveHTTPRequest(r.Method, state.route, rw.status, duration)

			attrs := []any{
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"operation", state.route,
				"status", rw.status,
				"outcome", requestOutcome(rw.status),
				"duration", duration.Milliseconds(),
			}
			if state.errCode != "" {
				attrs = append(attrs, "error_code", state.errCode)
			}
			h.logger.Info("http request", attrs...)
		}()

		next.ServeHTTP(rw, r)
	})
}
