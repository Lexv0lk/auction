package httpapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type requestIDKey struct{}

// WithRequestID stores the request ID in the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

// TryGetRequestID returns the request ID stored in the context, if any.
func TryGetRequestID(ctx context.Context) (string, bool) {
	requestID, ok := ctx.Value(requestIDKey{}).(string)

	return requestID, ok
}

func (h *Handler) enrichWithID(next http.Handler) http.Handler {
	next = h.recoverPanic(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var idBytes [16]byte
		if _, err := rand.Read(idBytes[:]); err != nil {
			h.logger.Error("request ID generation failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)

			return
		}

		requestID := hex.EncodeToString(idBytes[:])
		r = r.WithContext(WithRequestID(r.Context(), requestID))
		w.Header().Set("X-Request-ID", requestID)

		next.ServeHTTP(w, r)
	})
}
