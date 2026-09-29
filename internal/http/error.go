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

// Error renders the shared error answer: a JSON body for API routes and the
// layout error page for HTML routes. Technical causes (SQL errors, secrets,
// tokens) never reach the message; the request ID ties the answer to the log.
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
	data := errorData{
		pageData:  h.newPageData(r),
		Status:    status,
		Message:   message,
		RequestID: requestID,
	}
	data.Title = "Ошибка"

	if t, ok := h.pages["error.html"]; ok {
		_ = t.ExecuteTemplate(w, "layout", data)
	} else {
		h.logger.Error("error page template is missing")
	}
}
