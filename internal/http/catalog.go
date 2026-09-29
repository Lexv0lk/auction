package httpapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Lexv0lk/auction/internal/lot"
)

// catalogPage renders the participant catalog: published lots only, with the
// category and display-state filters and the fixed page size. Drafts are
// invisible here for every role; the administrator manages them through the
// administrative section.
func (h *Handler) catalogPage(w http.ResponseWriter, r *http.Request) {
	filter := catalogFilterFromQuery(r)

	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list categories", err)

		return
	}
	items, hasMore, err := h.lots.Catalog(r.Context(), filter)
	if err != nil {
		h.lotServiceError(w, r, "list catalog", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Каталог лотов"
	h.renderPage(w, r, http.StatusOK, "catalog.html", catalogData{
		pageData:   data,
		Items:      items,
		Categories: categories,
		CategoryID: filter.CategoryID,
		State:      filter.State,
		PrevURL:    catalogURL(filter, filter.Page-1),
		NextURL:    catalogURL(filter, filter.Page+1),
		HasPrev:    filter.Page > 1,
		HasNext:    hasMore,
	})
}

// catalogFilterFromQuery parses the GET filters forgivingly: a malformed
// value means "no filter" or "first page", never an error page, because a
// catalog link is always safe to open.
func catalogFilterFromQuery(r *http.Request) lot.CatalogFilter {
	filter := lot.CatalogFilter{Page: 1}
	if categoryID, err := strconv.ParseInt(r.FormValue("category"), 10, 64); err == nil && categoryID > 0 {
		filter.CategoryID = categoryID
	}
	filter.State = lot.NormalizeState(r.FormValue("state"))
	if page, err := strconv.Atoi(r.FormValue("page")); err == nil && page > 0 {
		filter.Page = page
	}

	return filter
}

// catalogURL rebuilds the catalog address for a pagination link, preserving
// the active filters.
func catalogURL(filter lot.CatalogFilter, page int) string {
	values := url.Values{}
	if filter.CategoryID > 0 {
		values.Set("category", strconv.FormatInt(filter.CategoryID, 10))
	}
	if filter.State != "" {
		values.Set("state", filter.State)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/lots?" + encoded
	}

	return "/lots"
}

// lotPublicPage renders the published lot page: the current conditions, the
// state, the result of a finished lot and the bid history with pagination.
// A draft answers as a missing lot for every role.
func (h *Handler) lotPublicPage(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.lotNotFound(w, r)

		return
	}
	h.renderLotPage(w, r, http.StatusOK, id, nil, bidNoticeFromQuery(r), "")
}

// renderLotPage loads the current lot state and renders the participant lot
// page. form == nil marks a plain page load: the bid form carries a newly
// issued request key. notice and errorText fill the page banner; a failed
// load falls back to the shared error answer, which the bool reports.
func (h *Handler) renderLotPage(w http.ResponseWriter, r *http.Request, status int, id int64, form *bidForm, notice, errorText string) bool {
	p, err := h.lots.GetPublicLot(r.Context(), id)
	if errors.Is(err, lot.ErrNotFound) {
		h.lotNotFound(w, r)

		return false
	}
	if err != nil {
		h.lotServiceError(w, r, "load public lot", err)

		return false
	}

	bidsPage := 1
	if page, err := strconv.Atoi(r.FormValue("bids")); err == nil && page > 0 {
		bidsPage = page
	}
	bids, bidsHasMore, err := h.lots.ListBids(r.Context(), id, bidsPage)
	if err != nil {
		h.lotServiceError(w, r, "list bids", err)

		return false
	}

	if form == nil {
		// Every rendered page issues the key of one fresh bid intention; a
		// resubmission of the same form repeats the same intention.
		form = &bidForm{RequestKey: lot.NewRequestKey()}
	}
	data := h.newPageData(r)
	data.Title = p.Title
	data.Error = errorText
	h.renderPage(w, r, status, "lot_page.html", lotPublicData{
		pageData:       data,
		Lot:            p,
		MinimumNextBid: publicMinimumNextBid(p),
		CanBid:         p.CanBid(),
		BidNotice:      notice,
		BidForm:        *form,
		Bids:           bids,
		BidsPage:       bidsPage,
		PrevBidsURL:    lotPageURL(id, bidsPage-1),
		NextBidsURL:    lotPageURL(id, bidsPage+1),
		BidsHasPrev:    bidsPage > 1,
		BidsHasNext:    bidsHasMore,
	})

	return true
}

// bidNoticeFromQuery turns the redirect marker of a successful form post
// into the page notice; any other value names no marker and shows nothing,
// so a hand-edited address cannot inject text.
func bidNoticeFromQuery(r *http.Request) string {
	if id := r.FormValue("placed"); isDecimalID(id) {
		return "Ставка принята: " + id + "."
	}
	if id := r.FormValue("replayed"); isDecimalID(id) {
		return "Эта ставка уже была учтена ранее (ID " + id + "): это повтор того же запроса."
	}

	return ""
}

// isDecimalID reports whether the value is a short decimal number: the only
// shape the redirect markers carry.
func isDecimalID(value string) bool {
	if value == "" || len(value) > 18 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}

	return true
}

func publicMinimumNextBid(p lot.PublicLot) *int64 {
	if minimum, ok := p.MinimumNextBid(); ok {
		return &minimum
	}

	return nil
}

func lotPageURL(id int64, bidsPage int) string {
	if bidsPage <= 1 {
		return "/lots/" + strconv.FormatInt(id, 10)
	}

	return "/lots/" + strconv.FormatInt(id, 10) + "?bids=" + strconv.Itoa(bidsPage)
}

// lotStateAPI answers the periodic state refresh of the lot page: the
// current state from one database statement, the server (database) time and
// the first portion of the history. Money values and identifiers are
// decimal strings, absent values are null; passwords, sessions and request
// keys never enter the answer.
func (h *Handler) lotStateAPI(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.Error(w, r, http.StatusNotFound, "lot_not_found", "Лот не найден")

		return
	}
	p, err := h.lots.GetPublicLot(r.Context(), id)
	if errors.Is(err, lot.ErrNotFound) {
		h.Error(w, r, http.StatusNotFound, "lot_not_found", "Лот не найден")

		return
	}
	if err != nil {
		h.lotServiceError(w, r, "load public lot", err)

		return
	}
	bids, bidsHasMore, err := h.lots.ListBids(r.Context(), id, 1)
	if err != nil {
		h.lotServiceError(w, r, "list bids", err)

		return
	}

	response := buildLotStateResponse(p, bids, bidsHasMore)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

type apiCategoryRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiWinningBid struct {
	ID          string `json:"id"`
	Amount      string `json:"amount"`
	Participant string `json:"participant"`
}

type apiHistoryBid struct {
	ID          string `json:"id"`
	Participant string `json:"participant"`
	Amount      string `json:"amount"`
	AcceptedAt  string `json:"accepted_at"`
}

type apiLotState struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Category           apiCategoryRef  `json:"category"`
	Status             string          `json:"status"`
	DisplayStatus      string          `json:"display_status"`
	ServerTime         string          `json:"server_time"`
	EndsAt             string          `json:"ends_at"`
	StartPrice         string          `json:"start_price"`
	CurrentPrice       string          `json:"current_price"`
	MinimumNextBid     *string         `json:"minimum_next_bid"`
	CanBid             bool            `json:"can_bid"`
	WinningBid         *apiWinningBid  `json:"winning_bid"`
	BidHistory         []apiHistoryBid `json:"bid_history"`
	BidHistoryHasMore  bool            `json:"bid_history_has_more"`
	BidHistoryNextPage *int            `json:"bid_history_next_page"`
}

func apiTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func apiAmount(value int64) string {
	return strconv.FormatInt(value, 10)
}

func buildLotStateResponse(p lot.PublicLot, bids []lot.Bid, bidsHasMore bool) apiLotState {
	response := apiLotState{
		ID:            apiAmount(p.ID),
		Title:         p.Title,
		Description:   p.Description,
		Category:      apiCategoryRef{ID: apiAmount(p.CategoryID), Name: p.CategoryName},
		Status:        p.Status,
		DisplayStatus: p.State,
		ServerTime:    apiTime(p.DatabaseNow),
		EndsAt:        apiTime(p.EndsAt),
		StartPrice:    apiAmount(p.StartPrice),
		CurrentPrice:  apiAmount(p.CurrentPrice),
		BidHistory:    make([]apiHistoryBid, 0, len(bids)),
	}
	if minimum, ok := p.MinimumNextBid(); ok {
		minimum := apiAmount(minimum)
		response.MinimumNextBid = &minimum
	}
	response.CanBid = p.CanBid()
	if p.WinningBidID != nil {
		response.WinningBid = &apiWinningBid{
			ID:          apiAmount(*p.WinningBidID),
			Amount:      apiAmount(p.WinningAmount),
			Participant: p.WinningParticipant,
		}
	}
	for _, bid := range bids {
		response.BidHistory = append(response.BidHistory, apiHistoryBid{
			ID:          apiAmount(bid.ID),
			Participant: bid.Participant,
			Amount:      apiAmount(bid.Amount),
			AcceptedAt:  apiTime(bid.AcceptedAt),
		})
	}
	if bidsHasMore {
		nextPage := 2
		response.BidHistoryHasMore = true
		response.BidHistoryNextPage = &nextPage
	}

	return response
}
