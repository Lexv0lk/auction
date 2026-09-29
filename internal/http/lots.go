package httpapp

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	// The deadline forms parse IANA time zone names; the embedded tzdata
	// keeps the zone database identical on every host (Windows has none) and
	// therefore identical on every replica.
	_ "time/tzdata"

	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
)

// User-visible messages of the lot forms and refusals.
const (
	lotTitleEmptyMessage         = "Введите название лота"
	lotTitleTooLongMessage       = "Название не может быть длиннее 200 символов"
	lotDescriptionEmptyMessage   = "Введите описание лота"
	lotDescriptionTooLongMessage = "Описание не может быть длиннее 5000 символов"
	lotCategoryRequiredMessage   = "Выберите категорию"
	lotCategoryMissingMessage    = "Выбранная категория не существует"
	lotPriceInvalidMessage       = "Стартовая цена должна быть целым положительным числом"
	lotEndsAtMissingMessage      = "Укажите дедлайн лота"
	lotEndsAtInvalidMessage      = "Введите корректные дату и время"
	lotTimeZoneInvalidMessage    = "Выберите часовой пояс из списка"
	lotNotDraftMessage           = "Условия лота неизменяемы: торги уже открыты или завершены, действие доступно только черновикам"
	lotNotPublishableMessage     = "Данные лота неполны, публикация невозможна"
	lotDeadlinePastMessage       = "Дедлайн должен быть в будущем по времени сервера; измените его в черновике"
)

// lotTimeZones is the explicit allowlist of input time zones: the browser
// submits datetime-local without a zone, so the form carries the zone in a
// separate field and the server accepts exactly the zones it offered.
var lotTimeZones = []string{
	"UTC",
	"Europe/Moscow",
	"Europe/Kaliningrad",
	"Europe/Samara",
	"Asia/Yekaterinburg",
	"Asia/Novosibirsk",
	"Asia/Krasnoyarsk",
	"Asia/Irkutsk",
	"Asia/Vladivostok",
}

var lotTimeZoneSet = func() map[string]struct{} {
	set := make(map[string]struct{}, len(lotTimeZones))
	for _, zone := range lotTimeZones {
		set[zone] = struct{}{}
	}

	return set
}()

// lotDeadlineDisplayLayout is the UTC display of stored deadlines:
// datetime-local shape for the form input, human shape for the lists.
const (
	lotDeadlineFormLayout = "2006-01-02T15:04"
	lotDeadlineListLayout = "02.01.2006 15:04"
)

// lotIDFromPath parses the {id} route segment; a missing or malformed ID
// names no resource and is answered as 404 without touching the service.
func lotIDFromPath(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)

	return id, err == nil && id > 0
}

func (h *Handler) lotsPage(w http.ResponseWriter, r *http.Request) {
	lots, err := h.lots.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list lots", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Лоты"
	h.renderPage(w, r, http.StatusOK, "lots.html", lotsData{pageData: data, Lots: lots})
}

func (h *Handler) lotNewPage(w http.ResponseWriter, r *http.Request) {
	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list categories", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Новый лот"
	form := newLotFormData(data, "Новый лот", "/admin/lots", categories, nil)
	h.renderPage(w, r, http.StatusOK, "lot_form.html", form)
}

// lotForm keeps the submitted strings of one form submission: parse failures
// re-render exactly what the administrator entered.
type lotForm struct {
	Title       string
	Description string
	CategoryID  string
	Price       string
	EndsAt      string
	EndsAtTZ    string
}

func lotFormFromRequest(r *http.Request) lotForm {
	return lotForm{
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		CategoryID:  r.PostFormValue("category_id"),
		Price:       r.PostFormValue("start_price"),
		EndsAt:      r.PostFormValue("ends_at"),
		EndsAtTZ:    r.PostFormValue("ends_at_tz"),
	}
}

// lotInputFromForm converts the submitted strings into the service input and
// collects per-field parse failures. The int64 range, the datetime format and
// the time zone allowlist are checked before any SQL runs; a datetime-local
// value is never interpreted without the explicitly chosen zone.
func lotInputFromForm(form lotForm) (lot.Input, map[string]string) {
	fieldErrors := map[string]string{}
	input := lot.Input{Title: form.Title, Description: form.Description}

	categoryID, err := strconv.ParseInt(form.CategoryID, 10, 64)
	if err != nil || categoryID <= 0 {
		fieldErrors["category_id"] = lotCategoryRequiredMessage
	} else {
		input.CategoryID = categoryID
	}

	price, err := strconv.ParseInt(form.Price, 10, 64)
	if err != nil {
		fieldErrors["start_price"] = lotPriceInvalidMessage
	} else {
		input.StartPrice = price
	}

	if form.EndsAt == "" {
		fieldErrors["ends_at"] = lotEndsAtMissingMessage
	} else {
		endsAt, err := parseLotDeadline(form.EndsAt, form.EndsAtTZ)
		switch {
		case errors.Is(err, errLotTimeZone):
			fieldErrors["ends_at_tz"] = lotTimeZoneInvalidMessage
		case err != nil:
			fieldErrors["ends_at"] = lotEndsAtInvalidMessage
		default:
			input.EndsAt = endsAt
		}
	}

	return input, fieldErrors
}

var (
	errLotTimeZone = errors.New("unknown time zone")
	errLotDateTime = errors.New("invalid datetime")
)

// parseLotDeadline converts the zone-less datetime-local string into an
// absolute instant using the explicitly submitted zone.
func parseLotDeadline(value, zone string) (time.Time, error) {
	if _, ok := lotTimeZoneSet[zone]; !ok {
		return time.Time{}, errLotTimeZone
	}

	location, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, errLotTimeZone
	}

	for _, layout := range []string{lotDeadlineFormLayout, "2006-01-02T15:04:05"} {
		if endsAt, err := time.ParseInLocation(layout, value, location); err == nil {
			return endsAt, nil
		}
	}

	return time.Time{}, errLotDateTime
}

func (h *Handler) lotCreate(w http.ResponseWriter, r *http.Request) {
	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list categories", err)

		return
	}

	form := lotFormFromRequest(r)
	input, fieldErrors := lotInputFromForm(form)
	status := http.StatusUnprocessableEntity
	if len(fieldErrors) == 0 {
		created, err := h.lots.Create(r.Context(), input)
		if err == nil {
			// The created draft is shown to the administrator right away.
			http.Redirect(w, r, "/admin/lots/"+strconv.FormatInt(created.ID, 10)+"/edit", http.StatusSeeOther)

			return
		}
		if !h.lotFormFailure(w, r, "create lot", err, fieldErrors, &status) {
			return
		}
	}

	data := h.newPageData(r)
	data.Title = "Новый лот"
	formData := newLotFormData(data, "Новый лот", "/admin/lots", categories, fieldErrors)
	formData.fillFromForm(form, input)
	h.renderPage(w, r, status, "lot_form.html", formData)
}

func (h *Handler) lotEditPage(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.lotNotFound(w, r)

		return
	}
	l, err := h.lots.Get(r.Context(), id)
	if errors.Is(err, lot.ErrNotFound) {
		h.lotNotFound(w, r)

		return
	}
	if err != nil {
		h.lotServiceError(w, r, "load lot", err)

		return
	}
	// Published and finished lots are immutable: offering their edit form
	// would promise an edit the operation refuses, so the direct request is
	// a conflict exactly like the POST.
	if l.Status != lot.StatusDraft {
		h.Error(w, r, http.StatusConflict, "conflict", lotNotDraftMessage)

		return
	}

	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list categories", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Изменение лота"
	action := "/admin/lots/" + strconv.FormatInt(id, 10)
	form := newLotFormData(data, "Изменение лота", action, categories, nil)
	form.LotID = id
	form.Title = l.Title
	form.Description = l.Description
	form.CategoryID = l.CategoryID
	form.Price = strconv.FormatInt(l.StartPrice, 10)
	// The form displays the stored absolute deadline in UTC and says so: the
	// zone select starts at UTC, what is shown is what will be stored.
	form.EndsAt = l.EndsAt.In(time.UTC).Format(lotDeadlineFormLayout)
	form.EndsAtTZ = "UTC"
	h.renderPage(w, r, http.StatusOK, "lot_form.html", form)
}

func (h *Handler) lotUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.lotNotFound(w, r)

		return
	}
	categories, err := h.categories.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list categories", err)

		return
	}

	form := lotFormFromRequest(r)
	input, fieldErrors := lotInputFromForm(form)
	status := http.StatusUnprocessableEntity
	if len(fieldErrors) == 0 {
		_, err := h.lots.Update(r.Context(), id, input)
		switch {
		case err == nil:
			// The redirect re-reads the updated lot: the administrator sees
			// exactly what the database stored.
			http.Redirect(w, r, "/admin/lots/"+strconv.FormatInt(id, 10)+"/edit", http.StatusSeeOther)

			return
		case errors.Is(err, lot.ErrNotFound):
			h.lotNotFound(w, r)

			return
		case errors.Is(err, lot.ErrNotDraft):
			h.lotListRefusal(w, r, http.StatusConflict, lotNotDraftMessage)

			return
		}
		if !h.lotFormFailure(w, r, "update lot", err, fieldErrors, &status) {
			return
		}
	}

	data := h.newPageData(r)
	data.Title = "Изменение лота"
	action := "/admin/lots/" + strconv.FormatInt(id, 10)
	formData := newLotFormData(data, "Изменение лота", action, categories, fieldErrors)
	formData.LotID = id
	formData.fillFromForm(form, input)
	h.renderPage(w, r, status, "lot_form.html", formData)
}

func (h *Handler) lotDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.lotNotFound(w, r)

		return
	}
	err := h.lots.Delete(r.Context(), id)
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/lots", http.StatusSeeOther)
	case errors.Is(err, lot.ErrNotFound):
		h.lotNotFound(w, r)
	case errors.Is(err, lot.ErrNotDraft):
		h.lotListRefusal(w, r, http.StatusConflict, lotNotDraftMessage)
	default:
		h.lotServiceError(w, r, "delete lot", err)
	}
}

func (h *Handler) lotPublish(w http.ResponseWriter, r *http.Request) {
	id, ok := lotIDFromPath(r)
	if !ok {
		h.lotNotFound(w, r)

		return
	}
	_, err := h.lots.Publish(r.Context(), id)
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/lots", http.StatusSeeOther)
	case errors.Is(err, lot.ErrNotFound):
		h.lotNotFound(w, r)
	case errors.Is(err, lot.ErrNotDraft):
		h.lotListRefusal(w, r, http.StatusConflict, lotNotDraftMessage)
	case errors.Is(err, lot.ErrNotPublishable):
		h.lotListRefusal(w, r, http.StatusConflict, lotNotPublishableMessage)
	case errors.Is(err, lot.ErrDeadlinePast):
		// The past deadline is a field-level problem of the draft: the lot
		// stays a draft and the deadline stays editable.
		h.lotListRefusal(w, r, http.StatusUnprocessableEntity, lotDeadlinePastMessage)
	default:
		h.lotServiceError(w, r, "publish lot", err)
	}
}

// lotListRefusal re-renders the administrative list with an explanation: the
// page the publish and delete buttons live on stays usable and shows the
// current statuses.
func (h *Handler) lotListRefusal(w http.ResponseWriter, r *http.Request, status int, message string) {
	lots, err := h.lots.List(r.Context())
	if err != nil {
		h.lotServiceError(w, r, "list lots", err)

		return
	}

	data := h.newPageData(r)
	data.Title = "Лоты"
	data.Error = message
	h.renderPage(w, r, status, "lots.html", lotsData{pageData: data, Lots: lots})
}

// lotFormFailure maps a failed create or update to the form answer: a
// user-visible field explanation for validation problems (422). Database
// failures stay technical answers and report handled=false.
func (h *Handler) lotFormFailure(w http.ResponseWriter, r *http.Request, operation string, err error, fieldErrors map[string]string, status *int) bool {
	switch {
	case errors.Is(err, lot.ErrTitleEmpty):
		fieldErrors["title"] = lotTitleEmptyMessage
	case errors.Is(err, lot.ErrTitleTooLong):
		fieldErrors["title"] = lotTitleTooLongMessage
	case errors.Is(err, lot.ErrDescriptionEmpty):
		fieldErrors["description"] = lotDescriptionEmptyMessage
	case errors.Is(err, lot.ErrDescriptionTooLong):
		fieldErrors["description"] = lotDescriptionTooLongMessage
	case errors.Is(err, lot.ErrCategoryRequired):
		fieldErrors["category_id"] = lotCategoryRequiredMessage
	case errors.Is(err, lot.ErrCategoryMissing):
		fieldErrors["category_id"] = lotCategoryMissingMessage
	case errors.Is(err, lot.ErrStartPriceNonPositive):
		fieldErrors["start_price"] = lotPriceInvalidMessage
	case errors.Is(err, lot.ErrEndsAtMissing):
		fieldErrors["ends_at"] = lotEndsAtMissingMessage
	default:
		h.lotServiceError(w, r, operation, err)

		return false
	}
	*status = http.StatusUnprocessableEntity

	return true
}

func (h *Handler) lotNotFound(w http.ResponseWriter, r *http.Request) {
	h.Error(w, r, http.StatusNotFound, "not_found", "Лот не найден")
}

func (h *Handler) lotServiceError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	h.logger.Error(operation, "error", err)
	h.Error(w, r, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен, попробуйте позже")
}

// lotFormData is the payload of the create and edit forms; the entered
// strings are kept as-is so a failed submission is never silently rewritten.
type lotFormData struct {
	pageData
	Heading     string
	FormAction  string
	Categories  []category.Category
	TimeZones   []string
	LotID       int64
	Title       string
	Description string
	CategoryID  int64
	Price       string
	EndsAt      string
	EndsAtTZ    string
	FieldErrors map[string]string
}

func newLotFormData(base pageData, heading, action string, categories []category.Category, fieldErrors map[string]string) lotFormData {
	if fieldErrors == nil {
		fieldErrors = map[string]string{}
	}

	return lotFormData{
		pageData:    base,
		Heading:     heading,
		FormAction:  action,
		Categories:  categories,
		TimeZones:   lotTimeZones,
		FieldErrors: fieldErrors,
		EndsAtTZ:    "UTC",
	}
}

// fillFromForm restores the submitted values for the re-rendered form.
func (d *lotFormData) fillFromForm(form lotForm, input lot.Input) {
	d.Title = form.Title
	d.Description = form.Description
	d.CategoryID = input.CategoryID
	d.Price = strings.TrimSpace(form.Price)
	d.EndsAt = form.EndsAt
	d.EndsAtTZ = form.EndsAtTZ
}
