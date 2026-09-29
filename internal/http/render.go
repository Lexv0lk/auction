package httpapp

import (
	"html/template"
	"net/http"

	"github.com/gorilla/csrf"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
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

type errorData struct {
	pageData
	Status    int
	Message   string
	RequestID string
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
	}
	parsed := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		t, err := template.New(name).ParseFS(web.Files, files...)
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
