package httpapp

import (
	"net/http"
	"time"
)

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

func (h *Handler) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w}
		start := time.Now()
		requestID, _ := TryGetRequestID(r.Context())

		defer func() {
			if value := recover(); value != nil {
				h.logger.Error("request panic", "request_id", requestID)
				if rw.status == 0 {
					h.Error(rw, r, http.StatusInternalServerError, "internal_error", "Внутренняя ошибка сервера")
				}
			}

			if rw.status == 0 {
				rw.status = http.StatusOK
			}

			h.logger.Info("http request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
		}()

		next.ServeHTTP(rw, r)
	})
}
