package httpapp

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
)

func draftLot() lot.Lot {
	return lot.Lot{
		ID: 7, Title: "Серебряный рубль", Description: "Монета в хорошем состоянии",
		CategoryID: 3, CategoryName: "Нумизматика", StartPrice: 5000,
		Status: lot.StatusDraft, EndsAt: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
	}
}

func activeLot() lot.Lot {
	active := draftLot()
	active.Status = lot.StatusActive

	return active
}

func testCategories() *fakeCategories {
	return &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return []category.Category{{ID: 3, Name: "Нумизматика"}, {ID: 4, Name: "Филателия"}}, nil
		},
	}
}

func lotFormValues() url.Values {
	return url.Values{
		"title":       {"Серебряный рубль"},
		"description": {"Монета в хорошем состоянии"},
		"category_id": {"3"},
		"start_price": {"5000"},
		"ends_at":     {"2026-10-05T12:00"},
		"ends_at_tz":  {"Europe/Moscow"},
	}
}

func withCSRF(values url.Values, token string) url.Values {
	values.Set("gorilla.csrf.Token", token)

	return values
}

// validatingCreate emulates the service contract: the input is validated
// before any storage is touched.
func validatingCreate(_ context.Context, input lot.Input) (lot.Lot, error) {
	if err := lot.ValidateInput(input); err != nil {
		return lot.Lot{}, err
	}

	return draftLot(), nil
}

func validatingUpdate(_ context.Context, _ int64, input lot.Input) (lot.Lot, error) {
	if err := lot.ValidateInput(input); err != nil {
		return lot.Lot{}, err
	}

	return draftLot(), nil
}

func TestLotsListRequiresAdmin(t *testing.T) {
	server, guest := newTestServer(t, &fakeAuthenticator{}, testConfig())
	resp := getRequest(t, guest, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	participant := auth.User{ID: 2, Login: "demo-participant-1", Role: auth.RoleParticipant}
	server, participantClient := loggedInClient(t, participant, testCategories())
	resp = getRequest(t, participantClient, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	fake := &fakeLots{}
	server, admin := adminClient(t, testCategories(), fake)
	resp = getRequest(t, admin, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, fake.listCalls)
}

func TestLotsListShowsAllStatusesAndDraftActions(t *testing.T) {
	fake := &fakeLots{listFunc: func(context.Context) ([]lot.Lot, error) {
		return []lot.Lot{draftLot(), activeLot()}, nil
	}}
	server, admin := adminClient(t, testCategories(), fake)

	resp := getRequest(t, admin, server.URL+"/admin/lots")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Contains(t, resp.Body, "Серебряный рубль")
	assert.Contains(t, resp.Body, "Нумизматика")
	assert.Contains(t, resp.Body, "черновик")
	assert.Contains(t, resp.Body, "торги идут")
	// Deadlines display in UTC with an explicit marker.
	assert.Contains(t, resp.Body, "05.10.2026 09:00 UTC")

	// Draft actions exist only for the draft.
	assert.Contains(t, resp.Body, "/admin/lots/7/edit")
	assert.Contains(t, resp.Body, "/admin/lots/7/publish")
	assert.Contains(t, resp.Body, "/admin/lots/7/delete")
	assert.NotContains(t, resp.Body, "/admin/lots/8/edit")
	assert.NotContains(t, resp.Body, "/admin/lots/8/publish")
}

func TestLotsListEmpty(t *testing.T) {
	server, admin := adminClient(t)

	resp := getRequest(t, admin, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Лотов пока нет")
}

func TestLotNewPageOffersCategories(t *testing.T) {
	server, admin := adminClient(t, testCategories())

	resp := getRequest(t, admin, server.URL+"/admin/lots/new")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `action="/admin/lots"`)
	assert.Contains(t, resp.Body, `value="3">Нумизматика</option>`)
	assert.Contains(t, resp.Body, "Europe/Moscow")
}

func TestLotCreateParsesFormWithTimezone(t *testing.T) {
	fake := &fakeLots{createFunc: func(context.Context, lot.Input) (lot.Lot, error) {
		return draftLot(), nil
	}}
	server, admin := adminClient(t, testCategories(), fake)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots/new")
	resp := postForm(t, admin, server.URL+"/admin/lots", withCSRF(lotFormValues(), token))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/lots/7/edit", resp.Header.Get("Location"), "the created draft is shown")
	assert.Equal(t, 1, fake.createCalls)

	input := fake.lastInput
	assert.Equal(t, "Серебряный рубль", input.Title)
	assert.Equal(t, int64(3), input.CategoryID)
	assert.Equal(t, int64(5000), input.StartPrice)
	// The entered wall time is 12:00 in Europe/Moscow: the absolute time is
	// 09:00 UTC.
	assert.True(t, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC).Equal(input.EndsAt),
		"the deadline is stored as the absolute instant of the chosen zone")
}

func TestLotCreateFailuresKeepInput(t *testing.T) {
	cases := []struct {
		name          string
		override      func(url.Values)
		field         string
		message       string
		reachesCreate bool
	}{
		{
			name:          "empty title",
			override:      func(v url.Values) { v.Set("title", "   ") },
			field:         "title",
			message:       "Введите название лота",
			reachesCreate: true,
		},
		{
			name:          "too long title",
			override:      func(v url.Values) { v.Set("title", strings.Repeat("р", lot.MaxTitleLength+1)) },
			field:         "title",
			message:       "длиннее 200 символов",
			reachesCreate: true,
		},
		{
			name:          "empty description",
			override:      func(v url.Values) { v.Set("description", "") },
			field:         "description",
			message:       "Введите описание лота",
			reachesCreate: true,
		},
		{
			name:          "zero price",
			override:      func(v url.Values) { v.Set("start_price", "0") },
			field:         "start_price",
			message:       "целым положительным числом",
			reachesCreate: true,
		},
		{
			name:          "fractional price",
			override:      func(v url.Values) { v.Set("start_price", "50.5") },
			field:         "start_price",
			message:       "целым положительным числом",
			reachesCreate: false,
		},
		{
			name: "overflowing price",
			override: func(v url.Values) {
				v.Set("start_price", strconv.FormatInt(9223372036854775807, 10)+"000")
			},
			field:         "start_price",
			message:       "целым положительным числом",
			reachesCreate: false,
		},
		{
			name:          "malformed category",
			override:      func(v url.Values) { v.Set("category_id", "abc") },
			field:         "category_id",
			message:       "Выберите категорию",
			reachesCreate: false,
		},
		{
			name:          "wrong date",
			override:      func(v url.Values) { v.Set("ends_at", "2026-13-45T99:00") },
			field:         "ends_at",
			message:       "корректные дату и время",
			reachesCreate: false,
		},
		{
			name:          "unknown timezone",
			override:      func(v url.Values) { v.Set("ends_at_tz", "Mars/Olympus") },
			field:         "ends_at_tz",
			message:       "часовой пояс",
			reachesCreate: false,
		},
		{
			name:          "missing deadline",
			override:      func(v url.Values) { v.Del("ends_at") },
			field:         "ends_at",
			message:       "Укажите дедлайн лота",
			reachesCreate: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeLots{createFunc: validatingCreate}
			server, admin := adminClient(t, testCategories(), fake)

			token := pageCSRFToken(t, admin, server.URL+"/admin/lots/new")
			values := lotFormValues()
			testCase.override(values)
			resp := postForm(t, admin, server.URL+"/admin/lots", withCSRF(values, token))

			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
			assert.Contains(t, resp.Body, testCase.message)
			assert.Contains(t, resp.Body, `action="/admin/lots"`, "the form is rendered again")
			if testCase.reachesCreate {
				assert.Equal(t, 1, fake.createCalls, "the service rejected the input")
			} else {
				assert.Equal(t, 0, fake.createCalls, "parse failures never reach the service")
			}

			// The entered strings come back so the admin can fix one field.
			if testCase.field != "title" {
				assert.Contains(t, resp.Body, `value="Серебряный рубль"`)
			}
		})
	}
}

func TestLotCreateUnknownCategoryFromService(t *testing.T) {
	fake := &fakeLots{createFunc: func(context.Context, lot.Input) (lot.Lot, error) {
		return lot.Lot{}, lot.ErrCategoryMissing
	}}
	server, admin := adminClient(t, testCategories(), fake)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots/new")
	resp := postForm(t, admin, server.URL+"/admin/lots", withCSRF(lotFormValues(), token))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, resp.Body, "не существует")
}

func TestLotEditPagePrefillsDraft(t *testing.T) {
	fake := &fakeLots{getFunc: func(context.Context, int64) (lot.Lot, error) {
		return draftLot(), nil
	}}
	server, admin := adminClient(t, testCategories(), fake)

	resp := getRequest(t, admin, server.URL+"/admin/lots/7/edit")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `action="/admin/lots/7"`)
	assert.Contains(t, resp.Body, `value="Серебряный рубль"`)
	assert.Contains(t, resp.Body, `value="5000"`)
	// The deadline is shown in UTC: the form states the zone explicitly.
	assert.Contains(t, resp.Body, `value="2026-10-05T09:00"`)
	assert.Contains(t, resp.Body, `value="3" selected>Нумизматика</option>`)
	assert.Equal(t, int64(7), fake.lastID)
}

func TestLotEditPageRefusesNonDraftAndMissing(t *testing.T) {
	fake := &fakeLots{getFunc: func(context.Context, int64) (lot.Lot, error) {
		return activeLot(), nil
	}}
	server, admin := adminClient(t, testCategories(), fake)

	resp := getRequest(t, admin, server.URL+"/admin/lots/7/edit")
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	fake.getFunc = func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, lot.ErrNotFound }
	resp = getRequest(t, admin, server.URL+"/admin/lots/987654/edit")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLotUpdateSavesAndRedirectsToTheLot(t *testing.T) {
	fake := &fakeLots{
		getFunc:    func(context.Context, int64) (lot.Lot, error) { return draftLot(), nil },
		updateFunc: func(context.Context, int64, lot.Input) (lot.Lot, error) { return draftLot(), nil },
	}
	server, admin := adminClient(t, testCategories(), fake)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots/7/edit")
	resp := postForm(t, admin, server.URL+"/admin/lots/7", withCSRF(lotFormValues(), token))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/lots/7/edit", resp.Header.Get("Location"), "the updated lot is shown after the redirect")
	assert.Equal(t, int64(7), fake.lastID)
	assert.Equal(t, int64(5000), fake.lastInput.StartPrice)
}

func TestLotUpdateValidationAndConflicts(t *testing.T) {
	fake := &fakeLots{
		getFunc:    func(context.Context, int64) (lot.Lot, error) { return draftLot(), nil },
		updateFunc: validatingUpdate,
	}
	server, admin := adminClient(t, testCategories(), fake)

	// The token page's cookie covers every /admin/lots/... POST target,
	// including the unknown-lot case below.
	token := pageCSRFToken(t, admin, server.URL+"/admin/lots/new")
	values := lotFormValues()
	values.Set("start_price", "0")
	resp := postForm(t, admin, server.URL+"/admin/lots/7", withCSRF(values, token))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, resp.Body, "целым положительным числом")
	assert.Equal(t, 1, fake.updateCalls, "the service rejected the input")

	fake.updateFunc = func(context.Context, int64, lot.Input) (lot.Lot, error) { return lot.Lot{}, lot.ErrNotDraft }
	resp = postForm(t, admin, server.URL+"/admin/lots/7", withCSRF(lotFormValues(), token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "черновик", "the refusal explains the draft-only rule")

	fake.updateFunc = func(context.Context, int64, lot.Input) (lot.Lot, error) { return lot.Lot{}, lot.ErrNotFound }
	resp = postForm(t, admin, server.URL+"/admin/lots/987654", withCSRF(lotFormValues(), token))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLotPublish(t *testing.T) {
	fake := &fakeLots{publishFunc: func(context.Context, int64) (lot.Lot, error) {
		return activeLot(), nil
	}}
	server, admin := adminClient(t, fake)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots")
	resp := postForm(t, admin, server.URL+"/admin/lots/7/publish", withCSRF(url.Values{}, token))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/lots", resp.Header.Get("Location"))
	assert.Equal(t, 1, fake.publishCalls)

	// A past deadline is a field-level failure: the draft stays editable.
	fake.publishFunc = func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, lot.ErrDeadlinePast }
	resp = postForm(t, admin, server.URL+"/admin/lots/7/publish", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, resp.Body, "будущем")

	// Republication is a state conflict.
	fake.publishFunc = func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, lot.ErrNotDraft }
	resp = postForm(t, admin, server.URL+"/admin/lots/7/publish", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	fake.publishFunc = func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, lot.ErrNotFound }
	resp = postForm(t, admin, server.URL+"/admin/lots/987654/publish", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLotDelete(t *testing.T) {
	fake := &fakeLots{}
	server, admin := adminClient(t, fake)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots")
	resp := postForm(t, admin, server.URL+"/admin/lots/7/delete", withCSRF(url.Values{}, token))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, 1, fake.deleteCalls)

	fake.deleteFunc = func(context.Context, int64) error { return lot.ErrNotDraft }
	resp = postForm(t, admin, server.URL+"/admin/lots/7/delete", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	fake.deleteFunc = func(context.Context, int64) error { return lot.ErrNotFound }
	resp = postForm(t, admin, server.URL+"/admin/lots/987654/delete", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLotServiceFailuresAreTechnical(t *testing.T) {
	fake := &fakeLots{
		listFunc:    func(context.Context) ([]lot.Lot, error) { return nil, errDatabaseDown },
		getFunc:     func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, errDatabaseDown },
		createFunc:  func(context.Context, lot.Input) (lot.Lot, error) { return lot.Lot{}, errDatabaseDown },
		publishFunc: func(context.Context, int64) (lot.Lot, error) { return lot.Lot{}, errDatabaseDown },
	}
	server, admin := adminClient(t, testCategories(), fake)

	resp := getRequest(t, admin, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	resp = getRequest(t, admin, server.URL+"/admin/lots/7/edit")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// The new-page form still renders, so the POST carries a fresh CSRF token
	// even while the lot service is down.
	token := pageCSRFToken(t, admin, server.URL+"/admin/lots/new")
	resp = postForm(t, admin, server.URL+"/admin/lots", withCSRF(lotFormValues(), token))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	resp = postForm(t, admin, server.URL+"/admin/lots/7/publish", withCSRF(url.Values{}, token))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotContains(t, resp.Body, "connection refused", "technical details stay out of the response")
}

func TestLotFormsAreProtectedByCSRF(t *testing.T) {
	fake := &fakeLots{}
	server, admin := adminClient(t, fake)

	resp := postForm(t, admin, server.URL+"/admin/lots", lotFormValues())
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Zero(t, fake.createCalls, "no create without CSRF")

	resp = postForm(t, admin, server.URL+"/admin/lots/7/publish", url.Values{})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Zero(t, fake.publishCalls, "no publish without CSRF")

	resp = postForm(t, admin, server.URL+"/admin/lots/7/delete", url.Values{})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Zero(t, fake.deleteCalls, "no delete without CSRF")
}
