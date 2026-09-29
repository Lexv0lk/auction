package httpapp

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Lexv0lk/auction/internal/category"
)

// User-visible field messages of the category forms; the list page and the
// edit form render them next to the name input.
const (
	categoryNameEmptyMessage  = "Введите название категории"
	categoryNameTooLong       = "Название не может быть длиннее 120 символов"
	categoryNameExistsMessage = "Категория с таким названием уже существует"
	categoryInUseMessage      = "Категория используется в лотах и не может быть удалена"
)

// categoryIDFromPath parses the {id} route segment; a missing or malformed ID
// names no resource and is answered as 404 without touching the service.
func categoryIDFromPath(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)

	return id, err == nil && id > 0
}

func (h *Handler) categoriesPage(w http.ResponseWriter, r *http.Request) {
	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.categoryServiceError(w, r, "list categories", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Категории"
	h.renderPage(w, r, http.StatusOK, "categories.html", categoriesData{pageData: data, Categories: categories})
}

func (h *Handler) categoryCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	_, err := h.categories.Create(r.Context(), name)
	if err != nil {
		fieldError, status, handled := h.categoryFieldFailure(w, r, "save category", err)
		if !handled {
			return
		}
		// The failure answer is the full list page: form, explanation and the
		// current reference data together.
		categories, listErr := h.categories.List(r.Context())
		if listErr != nil {
			h.categoryServiceError(w, r, "list categories", listErr)

			return
		}

		data := h.newPageData(r)
		data.Title = "Категории"
		h.renderPage(w, r, status, "categories.html",
			categoriesData{pageData: data, Categories: categories, Name: name, FieldError: fieldError})

		return
	}

	http.Redirect(w, r, "/admin/categories", http.StatusSeeOther)
}

func (h *Handler) categoryEditPage(w http.ResponseWriter, r *http.Request) {
	id, ok := categoryIDFromPath(r)
	if !ok {
		h.categoryNotFound(w, r)

		return
	}
	c, err := h.categories.Get(r.Context(), id)
	if errors.Is(err, category.ErrNotFound) {
		h.categoryNotFound(w, r)

		return
	}
	if err != nil {
		h.categoryServiceError(w, r, "load category", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Изменение категории"
	h.renderPage(w, r, http.StatusOK, "category_edit.html", categoryEditData{pageData: data, Category: c, Name: c.Name})
}

func (h *Handler) categoryRename(w http.ResponseWriter, r *http.Request) {
	id, ok := categoryIDFromPath(r)
	if !ok {
		h.categoryNotFound(w, r)

		return
	}
	name := r.PostFormValue("name")
	_, err := h.categories.Rename(r.Context(), id, name)
	if errors.Is(err, category.ErrNotFound) {
		h.categoryNotFound(w, r)

		return
	}
	if err != nil {
		fieldError, status, handled := h.categoryFieldFailure(w, r, "save category", err)
		if !handled {
			return
		}

		data := h.newPageData(r)
		data.Title = "Изменение категории"
		h.renderPage(w, r, status, "category_edit.html",
			categoryEditData{pageData: data, Category: category.Category{ID: id}, Name: name, FieldError: fieldError})

		return
	}

	http.Redirect(w, r, "/admin/categories", http.StatusSeeOther)
}

func (h *Handler) categoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := categoryIDFromPath(r)
	if !ok {
		h.categoryNotFound(w, r)

		return
	}
	err := h.categories.Delete(r.Context(), id)
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/categories", http.StatusSeeOther)
	case errors.Is(err, category.ErrNotFound):
		h.categoryNotFound(w, r)
	case errors.Is(err, category.ErrInUse):
		// The refusal needs the list again: the page stays usable and
		// explains why the category stays.
		categories, listErr := h.categories.List(r.Context())
		if listErr != nil {
			h.categoryServiceError(w, r, "list categories", listErr)

			return
		}
		data := h.newPageData(r)
		data.Title = "Категории"
		data.Error = categoryInUseMessage
		h.renderPage(w, r, http.StatusConflict, "categories.html", categoriesData{pageData: data, Categories: categories})
	default:
		h.categoryServiceError(w, r, "delete category", err)
	}
}

// categoryFieldFailure maps a failed create or rename to the form answer: a
// user-visible field explanation with the entered name for validation
// problems (422) and the unique-name conflict (409). Database failures stay
// technical answers and report handled=false.
func (h *Handler) categoryFieldFailure(w http.ResponseWriter, r *http.Request, operation string, err error) (fieldError string, status int, handled bool) {
	switch {
	case errors.Is(err, category.ErrNameEmpty):
		return categoryNameEmptyMessage, http.StatusUnprocessableEntity, true
	case errors.Is(err, category.ErrNameTooLong):
		return categoryNameTooLong, http.StatusUnprocessableEntity, true
	case errors.Is(err, category.ErrExists):
		return categoryNameExistsMessage, http.StatusConflict, true
	default:
		h.categoryServiceError(w, r, operation, err)

		return "", 0, false
	}
}

func (h *Handler) categoryNotFound(w http.ResponseWriter, r *http.Request) {
	h.Error(w, r, http.StatusNotFound, "not_found", "Категория не найдена")
}

func (h *Handler) categoryServiceError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	h.logger.Error(operation, "error", err)
	h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен, попробуйте позже")
}
