package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/observability"
)

// User-visible messages of the bid answers. The API message travels in the
// JSON error body, the HTML message into the re-rendered lot page; both name
// the outcome, neither names a technical cause.
const (
	bidTooLowHTMLMessage       = "Ставка не принята: сумма ниже минимальной ставки; проверьте актуальные данные лота."
	bidClosedHTMLMessage       = "Ставка не принята: торги закрыты."
	bidKeyConflictHTMLMessage  = "Ставка не принята: ключ запроса уже использован с другой суммой; обновите страницу для новой ставки."
	bidAmountInvalidMessage    = "Сумма ставки должна быть целым положительным числом"
	bidKeyInvalidMessage       = "Некорректный ключ запроса; обновите страницу и повторите."
	bidCommitUnknownMessage    = "Результат отправки неизвестен: повторите отправку той же суммы — повтор безопасен."
	bidParticipantDenyMessage  = "Недостаточно прав для ставки"
	bidAPIServiceDownMessage   = "Сервис временно недоступен, попробуйте позже"
	bidAPITooLowMessage        = "Ставка не принята: сумма ниже минимальной ставки"
	bidAPIClosedMessage        = "Ставка не принята: торги закрыты"
	bidAPIKeyConflictMessage   = "Ключ запроса уже использован с другой суммой" //nolint:gosec // G101 // user-visible text about the bid request key, not a credential
	bidAPIKeyInvalidMessage    = "Некорректный ключ запроса"                    //nolint:gosec // G101 // user-visible text about the bid request key, not a credential
	bidAPIBodyInvalidMessage   = "Некорректное тело запроса"
	bidAPICommitUnknownMessage = "Результат ставки неизвестен; повторите отправку с тем же ключом" //nolint:gosec // G101 // user-visible text about the bid request key, not a credential
)

// bidRefusal is one mapped service outcome, ready for both answer shapes:
// the API answers the code and the message, the HTML page re-renders with
// the HTML message and the preserved form.
type bidRefusal struct {
	status  int
	code    string
	apiMsg  string
	htmlMsg string
}

// mapBidFailure translates a PlaceBid failure into the HTTP contract. The
// bool reports whether the error is one of the known outcomes; anything
// else is a technical failure the caller answers as a temporary
// unavailability without details.
func mapBidFailure(err error) (bidRefusal, bool) {
	switch {
	case errors.Is(err, lot.ErrBidTooLow):
		return bidRefusal{http.StatusConflict, "bid_too_low", bidAPITooLowMessage, bidTooLowHTMLMessage}, true
	case errors.Is(err, lot.ErrBiddingClosed):
		return bidRefusal{http.StatusConflict, "auction_closed", bidAPIClosedMessage, bidClosedHTMLMessage}, true
	case errors.Is(err, lot.ErrRequestKeyConflict):
		return bidRefusal{http.StatusConflict, "request_key_conflict", bidAPIKeyConflictMessage, bidKeyConflictHTMLMessage}, true
	case errors.Is(err, lot.ErrNotFound):
		return bidRefusal{http.StatusNotFound, "lot_not_found", "Лот не найден", "Лот не найден"}, true
	case errors.Is(err, lot.ErrBidAmountInvalid):
		return bidRefusal{http.StatusUnprocessableEntity, "invalid_bid", bidAmountInvalidMessage, bidAmountInvalidMessage}, true
	case errors.Is(err, lot.ErrRequestKeyInvalid):
		return bidRefusal{http.StatusUnprocessableEntity, "invalid_request_key", bidAPIKeyInvalidMessage, bidKeyInvalidMessage}, true
	case errors.Is(err, lot.ErrParticipantRequired), errors.Is(err, lot.ErrParticipantMissing):
		return bidRefusal{http.StatusForbidden, "forbidden", bidParticipantDenyMessage, bidParticipantDenyMessage}, true
	case errors.Is(err, lot.ErrCommitOutcomeUnknown):
		// The bid may or may not be stored: the answer refuses nothing and
		// asks for a repeat of the original request.
		return bidRefusal{http.StatusServiceUnavailable, "commit_outcome_unknown", bidAPICommitUnknownMessage, bidCommitUnknownMessage}, true
	}

	return bidRefusal{}, false
}

// parseBidAmount converts the submitted decimal string into the int64 amount
// of the bid contract: digits only, inside the int64 range, strictly
// positive. A fraction, a sign, an overflow or any other shape is refused
// instead of being silently truncated or rounded.
func parseBidAmount(raw string) (int64, bool) {
	if raw == "" || len(raw) > 19 {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}

	return value, true
}

// bidAPIRequest is the JSON body of a bid. The pointers distinguish a
// missing field from an empty one, and the string type refuses a JSON
// number: money never travels as float64.
type bidAPIRequest struct {
	Amount     *string `json:"amount"`
	RequestKey *string `json:"request_key"`
}

type apiPlacedBid struct {
	ID         string `json:"id"`
	LotID      string `json:"lot_id"`
	Amount     string `json:"amount"`
	AcceptedAt string `json:"accepted_at"`
}

type bidAPIResponse struct {
	Bid      apiPlacedBid `json:"bid"`
	Replayed bool         `json:"replayed"`
}

// placeBidInstrumented runs the single bid operation and records its outcome:
// a replay of an earlier accepted bid is never counted as a newly accepted
// one, every typed refusal keeps its limited reason, and the observed
// duration includes the wait for the lot row lock.
func (h *Handler) placeBidInstrumented(ctx context.Context, participantID, lotID, amount int64, requestKey string) (lot.PlacedBid, error) {
	started := time.Now()
	placed, err := h.lots.PlaceBid(ctx, participantID, lotID, amount, requestKey)
	h.metrics.ObserveBidTransaction(time.Since(started))

	switch {
	case err == nil && placed.Repeated:
		h.metrics.CountBidAttempt(observability.BidOutcomeReplayed, observability.BidReasonNone)
	case err == nil:
		h.metrics.CountBidAttempt(observability.BidOutcomeAccepted, observability.BidReasonNone)
	case errors.Is(err, lot.ErrCommitOutcomeUnknown):
		h.metrics.CountBidAttempt(observability.BidOutcomeTechnical, "commit_outcome_unknown")
	default:
		if refusal, mapped := mapBidFailure(err); mapped {
			h.metrics.CountBidAttempt(observability.BidOutcomeRejected, refusal.code)
		} else {
			h.metrics.CountBidAttempt(observability.BidOutcomeTechnical, observability.BidReasonInternal)
		}
	}

	return placed, err
}

// countRejectedBid records an attempt refused before the service was called
// (a broken body, an unparseable amount, an unknown lot path).
func (h *Handler) countRejectedBid(reason string) {
	h.metrics.CountBidAttempt(observability.BidOutcomeRejected, reason)
}

// logAcceptedBid records the confirmed outcome after the operation returned:
// the replay outcome keeps replays distinguishable from new bids.
func (h *Handler) logAcceptedBid(outcome string, placed lot.PlacedBid) {
	h.logger.Info("bid accepted", "operation", "place_bid", "outcome", outcome,
		"bid_id", placed.ID, "lot_id", placed.LotID,
		"participant_id", placed.ParticipantID, "amount", placed.Amount,
		"replayed", placed.Repeated, "duration", placed.Duration.Milliseconds())
}

// logBidRefusal records a mapped business refusal with its limited reason.
func (h *Handler) logBidRefusal(lotID int64, code string) {
	h.logger.Warn("bid refused", "operation", "place_bid", "outcome", observability.BidOutcomeRejected,
		"lot_id", lotID, "error_code", code)
}

// logBidTechnicalError records an unmapped technical failure; the error text
// is redacted by the logger before it reaches the stream.
func (h *Handler) logBidTechnicalError(lotID int64, err error) {
	h.logger.Error("place bid", "operation", "place_bid", "outcome", observability.BidOutcomeTechnical,
		"lot_id", lotID, "error", err)
}

// bidAPISubmit serves POST /api/lots/{id}/bids. The participant identity
// comes from the session alone: no request field names a user, and an
// unknown body field is a refusal, not a silently ignored attempt.
func (h *Handler) bidAPISubmit(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.countRejectedBid("lot_not_found")
		h.Error(w, r, http.StatusNotFound, "lot_not_found", "Лот не найден")

		return
	}
	user, _ := auth.UserFromContext(r.Context())

	var request bidAPIRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.countRejectedBid("request_too_large")
			h.Error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "Слишком большое тело запроса")

			return
		}
		h.countRejectedBid("invalid_body")
		h.Error(w, r, http.StatusUnprocessableEntity, "invalid_body", bidAPIBodyInvalidMessage)

		return
	}
	if request.Amount == nil || request.RequestKey == nil {
		h.countRejectedBid("invalid_body")
		h.Error(w, r, http.StatusUnprocessableEntity, "invalid_body", bidAPIBodyInvalidMessage)

		return
	}
	amount, valid := parseBidAmount(*request.Amount)
	if !valid {
		h.countRejectedBid("invalid_bid")
		h.Error(w, r, http.StatusUnprocessableEntity, "invalid_bid", bidAmountInvalidMessage)

		return
	}

	placed, err := h.placeBidInstrumented(r.Context(), user.ID, id, amount, *request.RequestKey)
	if err != nil {
		h.answerBidAPIFailure(w, r, id, err)

		return
	}

	outcome := observability.BidOutcomeAccepted
	status := http.StatusCreated
	if placed.Repeated {
		outcome = observability.BidOutcomeReplayed
		status = http.StatusOK
	}
	h.logAcceptedBid(outcome, placed)
	response := bidAPIResponse{
		Bid: apiPlacedBid{
			ID:         apiAmount(placed.ID),
			LotID:      apiAmount(placed.LotID),
			Amount:     apiAmount(placed.Amount),
			AcceptedAt: apiTime(placed.AcceptedAt),
		},
		Replayed: placed.Repeated,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

// answerBidAPIFailure maps a service refusal onto the JSON error contract;
// unmapped errors stay technical answers without details.
func (h *Handler) answerBidAPIFailure(w http.ResponseWriter, r *http.Request, lotID int64, err error) {
	refusal, mapped := mapBidFailure(err)
	if !mapped {
		h.logBidTechnicalError(lotID, err)
		h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", bidAPIServiceDownMessage)

		return
	}
	h.logBidRefusal(lotID, refusal.code)
	h.Error(w, r, refusal.status, refusal.code, refusal.apiMsg)
}

// bidFormSubmit serves POST /lots/{id}/bids: the same PlaceBid operation as
// the API, answered as a page. Success redirects to the lot page with an
// outcome marker; every refusal re-renders the page with the entered amount
// and the same intention key, so a resubmission repeats the same request.
func (h *Handler) bidFormSubmit(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.countRejectedBid("lot_not_found")
		h.lotNotFound(w, r)

		return
	}
	user, _ := auth.UserFromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.countRejectedBid("request_too_large")
			h.Error(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "Слишком большое тело запроса")
		} else {
			h.countRejectedBid("invalid_request")
			h.Error(w, r, http.StatusBadRequest, "invalid_request", "Некорректный запрос")
		}

		return
	}

	submitted := bidForm{Amount: r.PostFormValue("amount"), RequestKey: r.PostFormValue("request_key")}
	amount, valid := parseBidAmount(submitted.Amount)
	if !valid {
		h.countRejectedBid("invalid_bid")
		h.renderBidFailure(w, r, http.StatusUnprocessableEntity, id, bidAmountInvalidMessage, "invalid_bid", submitted)

		return
	}

	placed, err := h.placeBidInstrumented(r.Context(), user.ID, id, amount, submitted.RequestKey)
	if err == nil {
		outcome := observability.BidOutcomeAccepted
		marker := "placed"
		if placed.Repeated {
			outcome = observability.BidOutcomeReplayed
			marker = "replayed"
		}
		h.logAcceptedBid(outcome, placed)

		http.Redirect(w, r, "/lots/"+strconv.FormatInt(id, 10)+"?"+marker+"="+strconv.FormatInt(placed.ID, 10), http.StatusSeeOther)

		return
	}

	preserved := submitted
	if errors.Is(err, lot.ErrRequestKeyInvalid) {
		// A key that can never be accepted is replaced by a fresh intention;
		// everything else keeps the submitted key for a safe repeat.
		preserved.RequestKey = lot.NewRequestKey()
	}
	refusal, mapped := mapBidFailure(err)
	if !mapped {
		h.logBidTechnicalError(id, err)
		refusal = bidRefusal{status: http.StatusServiceUnavailable, htmlMsg: bidAPIServiceDownMessage}
	} else {
		h.logBidRefusal(id, refusal.code)
	}
	if refusal.code == "lot_not_found" {
		h.lotNotFound(w, r)

		return
	}
	h.renderBidFailure(w, r, refusal.status, id, refusal.htmlMsg, refusal.code, preserved)
}

// renderBidFailure answers a refused or unknown bid by re-rendering the lot
// page: the participant keeps the entered amount and the intention key. If
// the lot data cannot be loaded anymore, the shared error answer takes over.
func (h *Handler) renderBidFailure(w http.ResponseWriter, r *http.Request, status int, lotID int64, message, code string, form bidForm) {
	routeStateFrom(r.Context()).setErrorCode(code)
	h.renderLotPage(w, r, status, lotID, &form, "", message)
}
