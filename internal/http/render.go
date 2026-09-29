package httpapp

import (
	"html/template"
	"net/http"
	"time"

	"github.com/gorilla/csrf"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/web"
)

// pageData is the payload of the shared layout: role-aware navigation and the
// user-visible error line. Per-page data structs embed it.
type pageData struct {
	Title     string
	User      *auth.User
	RoleLabel string
	CSRFToken template.HTML
	Error     string
}

type loginData struct {
	pageData
	Login string
}

type homeData struct {
	pageData
}

type categoriesData struct {
	pageData
	Categories []category.Category
	// Name keeps the entered value of a failed create; FieldError explains
	// why the form came back.
	Name       string
	FieldError string
}

type categoryEditData struct {
	pageData
	Category   category.Category
	Name       string
	FieldError string
}

type lotsData struct {
	pageData
	Lots []lot.Lot
}

// catalogData is the payload of the participant catalog; the filters echo
// the current selection so the form stays filled.
type catalogData struct {
	pageData
	Items      []lot.CatalogItem
	Categories []category.Category
	CategoryID int64
	State      string
	PrevURL    string
	NextURL    string
	HasPrev    bool
	HasNext    bool
}

// lotPublicData is the payload of the published lot page; MinimumNextBid is
// nil when the state accepts no next bid (closed bidding or the int64
// maximum reached).
type lotPublicData struct {
	pageData
	Lot            lot.PublicLot
	MinimumNextBid *int64
	CanBid         bool
	Bids           []lot.Bid
	BidsPage       int
	PrevBidsURL    string
	NextBidsURL    string
	BidsHasPrev    bool
	BidsHasNext    bool
}

type errorData struct {
	pageData
	Status    int
	Message   string
	RequestID string
}

// templateFuncs adds the presentation helpers shared by the pages: the lot
// deadlines always display in UTC (the zone is part of the text), the status
// labels are the user-visible names of the fixed lot statuses, and the
// display-state labels distinguish the running bidding from the result
// determination of an overdue active lot.
var templateFuncs = template.FuncMap{
	"fmtUTC": func(t time.Time) string {
		return t.In(time.UTC).Format("02.01.2006 15:04") + " UTC"
	},
	"statusLabel": func(status string) string {
		switch status {
		case lot.StatusDraft:
			return "черновик"
		case lot.StatusActive:
			return "торги идут"
		case lot.StatusFinished:
			return "торги завершены"
		default:
			return status
		}
	},
	"displayStateLabel": func(state string) string {
		switch state {
		case lot.DisplayStateActive:
			return "торги идут"
		case lot.DisplayStateDetermining:
			return "торги завершены, определяется результат"
		case lot.DisplayStateFinished:
			return "торги завершены"
		default:
			return state
		}
	},
	"rfc3339": func(t time.Time) string {
		return t.UTC().Format(time.RFC3339)
	},
}

// roleLabels maps the fixed account roles to their user-visible names; an
// unknown role renders without a label instead of crashing the page.
var roleLabels = map[string]string{
	auth.RoleAdmin:       "администратор",
	auth.RoleParticipant: "участник",
}

// pageTemplates maps the render name of every page to the layout parsed
// together with exactly that page's content block.
func parsePageTemplates() (map[string]*template.Template, error) {
	pages := map[string][]string{
		"error.html":         {"templates/layout.gohtml", "templates/error.gohtml"},
		"login.html":         {"templates/layout.gohtml", "templates/login.gohtml"},
		"home.html":          {"templates/layout.gohtml", "templates/home.gohtml"},
		"categories.html":    {"templates/layout.gohtml", "templates/categories.gohtml"},
		"category_edit.html": {"templates/layout.gohtml", "templates/category_edit.gohtml"},
		"lots.html":          {"templates/layout.gohtml", "templates/lots.gohtml"},
		"lot_form.html":      {"templates/layout.gohtml", "templates/lot_form.gohtml"},
		"catalog.html":       {"templates/layout.gohtml", "templates/catalog.gohtml"},
		"lot_page.html":      {"templates/layout.gohtml", "templates/lot_page.gohtml"},
	}
	parsed := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		t, err := template.New(name).Funcs(templateFuncs).ParseFS(web.Files, files...)
		if err != nil {
			return nil, err
		}
		parsed[name] = t
	}

	return parsed, nil
}

// newPageData fills the shared layout payload for the current request. User
// fields come from the authenticated context and are rendered through
// html/template auto-escaping.
func (h *Handler) newPageData(r *http.Request) pageData {
	data := pageData{CSRFToken: csrf.TemplateField(r)}
	if user, ok := auth.UserFromContext(r.Context()); ok {
		data.User = &user
		data.RoleLabel = roleLabels[user.Role]
	}

	return data
}

func (h *Handler) renderPage(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	t, ok := h.pages[name]
	if !ok {
		h.logger.Error("page template is missing", "template", name)
		h.Error(w, r, http.StatusInternalServerError, "internal_error", "Внутренняя ошибка сервера")

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		h.logger.Error("render page", "template", name, "error", err)
	}
}
