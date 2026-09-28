package httpapp

import (
	"encoding/json"
	"net/http"
	"strings"
)

type errorResponse struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (h *Handler) Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	requestID, _ := TryGetRequestID(r.Context())

	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		response := errorResponse{}
		response.Error.Code, response.Error.Message, response.Error.RequestID = code, message, requestID
		_ = json.NewEncoder(w).Encode(response)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	_ = h.errorTemplate.Execute(w, struct {
		Status             int
		Message, RequestID string
	}{status, message, requestID})
}
