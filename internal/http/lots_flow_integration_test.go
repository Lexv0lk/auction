//go:build integration

package httpapp

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/lot"
	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/testutil"
)

type rawLotRow struct {
	title       string
	description string
	categoryID  int64
	startPrice  int64
	status      string
	endsAt      time.Time
}

func loadRawLot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) (rawLotRow, bool) {
	t.Helper()

	var row rawLotRow
	err := pool.QueryRow(ctx,
		"SELECT title, description, category_id, start_price, status, ends_at FROM lots WHERE id = $1", id).
		Scan(&row.title, &row.description, &row.categoryID, &row.startPrice, &row.status, &row.endsAt)
	if err != nil {
		return rawLotRow{}, false
	}
	require.NoError(t, err)

	return row, true
}

func lotFormFrom(values url.Values) url.Values {
	form := url.Values{
		"title":       {values.Get("title")},
		"description": {values.Get("description")},
		"category_id": {values.Get("category_id")},
		"start_price": {values.Get("start_price")},
		"ends_at":     {values.Get("ends_at")},
		"ends_at_tz":  {values.Get("ends_at_tz")},
	}
	if form.Get("ends_at_tz") == "" {
		form.Set("ends_at_tz", "UTC")
	}

	return form
}

// postLotForm takes the CSRF token from the new-lot page: its cookie covers
// every /admin/lots/... POST target.
func postLotForm(t *testing.T, client *http.Client, server *httptest.Server, target string, values url.Values) *httpResponse {
	t.Helper()

	token := pageCSRFToken(t, client, server.URL+"/admin/lots/new")
	form := lotFormFrom(values)
	form.Set("gorilla.csrf.Token", token)

	return postForm(t, client, server.URL+target, form)
}

// TestLotsAdminFlowAgainstPostgreSQL walks the full draft lifecycle through
// the real services, templates and PostgreSQL.
func TestLotsAdminFlowAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	hash, err := password.Hash("integration-pass")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO users (login, password_hash, role) VALUES ('flow-lot-admin', $1, 'admin')", hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO users (login, password_hash, role) VALUES ('flow-lot-participant', $1, 'participant')", hash)
	require.NoError(t, err)

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), lot.NewService(pool), nil, testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Access control against the real database: a guest is redirected, a
	// participant is refused even with a CSRF token, nothing is created.
	guest, err := cookiejar.New(nil)
	require.NoError(t, err)
	guestClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           guest,
	}
	resp := getRequest(t, guestClient, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	participant := loginClient(t, server, "flow-lot-participant", "integration-pass")
	resp = getRequest(t, participant, server.URL+"/admin/lots")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	home := getRequest(t, participant, server.URL+"/")
	require.Equal(t, http.StatusOK, home.StatusCode)
	match := csrfFieldPattern.FindStringSubmatch(home.Body)
	require.NotNil(t, match)
	resp = postForm(t, participant, server.URL+"/admin/lots", url.Values{
		"title": {"Взлом"}, "gorilla.csrf.Token": {match[1]},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a participant must not create lots")

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots").Scan(&count))
	assert.Zero(t, count, "refused requests never touch the lots")

	admin := loginClient(t, server, "flow-lot-admin", "integration-pass")

	// The reference data first: one category for the flow.
	resp = createCategory(t, admin, server, "Нумизматика")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	match = editLinkPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match)
	categoryID, err := strconv.ParseInt(match[1], 10, 64)
	require.NoError(t, err)

	resp = getRequest(t, admin, server.URL+"/admin/lots")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Лотов пока нет")

	// Create: the form carries the deadline with an explicit zone; the stored
	// value is the absolute instant (12:00 Moscow == 09:00 UTC).
	createForm := url.Values{
		"title":       {"  Серебряный рубль 1726 года "},
		"description": {"Монета в хорошем состоянии"},
		"category_id": {strconv.FormatInt(categoryID, 10)},
		"start_price": {"5000"},
		"ends_at":     {"2026-10-05T12:00"},
		"ends_at_tz":  {"Europe/Moscow"},
	}
	resp = postLotForm(t, admin, server, "/admin/lots", createForm)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, resp.Body)
	require.True(t, strings.HasPrefix(resp.Header.Get("Location"), "/admin/lots/"), "the created draft is shown")
	lotID, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(resp.Header.Get("Location"), "/admin/lots/"), "/edit"), 10, 64)
	require.NoError(t, err)

	expectedEndsAt := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	stored, ok := loadRawLot(t, ctx, pool, lotID)
	require.True(t, ok)
	assert.Equal(t, "Серебряный рубль 1726 года", stored.title, "the stored title is trimmed")
	assert.Equal(t, categoryID, stored.categoryID)
	assert.Equal(t, int64(5000), stored.startPrice)
	assert.Equal(t, lot.StatusDraft, stored.status)
	assert.True(t, expectedEndsAt.Equal(stored.endsAt), "the deadline is stored as the absolute instant of the chosen zone")

	// The edit form shows the stored deadline in UTC.
	resp = getRequest(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/edit")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `value="Серебряный рубль 1726 года"`)
	assert.Contains(t, resp.Body, `value="5000"`)
	assert.Contains(t, resp.Body, `value="2026-10-05T09:00"`)

	// Update rewrites every editable field of the draft.
	updateForm := url.Values{
		"title":       {"Серебряный рубль, грейд MS62"},
		"description": {"Монета в слабе"},
		"category_id": {strconv.FormatInt(categoryID, 10)},
		"start_price": {"7500"},
		"ends_at":     {"2026-10-06T18:30"},
		"ends_at_tz":  {"UTC"},
	}
	resp = postLotForm(t, admin, server, "/admin/lots/"+strconv.FormatInt(lotID, 10), updateForm)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, resp.Body)
	stored, ok = loadRawLot(t, ctx, pool, lotID)
	require.True(t, ok)
	assert.Equal(t, "Серебряный рубль, грейд MS62", stored.title)
	assert.Equal(t, "Монета в слабе", stored.description)
	assert.Equal(t, int64(7500), stored.startPrice)
	assert.Equal(t, lot.StatusDraft, stored.status)
	assert.True(t, time.Date(2026, 10, 6, 18, 30, 0, 0, time.UTC).Equal(stored.endsAt))

	// Invalid submissions are form answers, never writes.
	invalidCases := []struct {
		name     string
		override func(url.Values)
		message  string
	}{
		{
			name:     "empty title",
			override: func(v url.Values) { v.Set("title", "   ") },
			message:  "Введите название лота",
		},
		{
			name:     "overflowing price",
			override: func(v url.Values) { v.Set("start_price", "99999999999999999999999") },
			message:  "целым положительным числом",
		},
		{
			name:     "zero price",
			override: func(v url.Values) { v.Set("start_price", "0") },
			message:  "целым положительным числом",
		},
		{
			name:     "wrong date",
			override: func(v url.Values) { v.Set("ends_at", "2026-13-45T99:00") },
			message:  "корректные дату и время",
		},
		{
			name:     "unknown timezone",
			override: func(v url.Values) { v.Set("ends_at_tz", "Mars/Olympus") },
			message:  "часовой пояс",
		},
		{
			name:     "unknown category",
			override: func(v url.Values) { v.Set("category_id", "999999") },
			message:  "не существует",
		},
	}
	for _, invalidCase := range invalidCases {
		t.Run("invalid "+invalidCase.name, func(t *testing.T) {
			// Each subtest overrides its own copy: the overrides must not
			// accumulate across cases.
			values := updateForm.Clone()
			invalidCase.override(values)
			resp := postLotForm(t, admin, server, "/admin/lots/"+strconv.FormatInt(lotID, 10), values)
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
			assert.Contains(t, resp.Body, invalidCase.message)
			assert.Contains(t, resp.Body, `value="`+values.Get("title")+`"`, "the entered title comes back")
		})
	}
	stored, ok = loadRawLot(t, ctx, pool, lotID)
	require.True(t, ok, "the invalid submissions changed nothing")
	assert.Equal(t, "Серебряный рубль, грейд MS62", stored.title)
	assert.Equal(t, int64(7500), stored.startPrice)

	// A past-deadline draft is created, refuses the publication and stays a
	// draft that can be fixed and published later.
	pastForm := url.Values{
		"title":       {"Просроченный черновик"},
		"description": {"Дедлайн в прошлом"},
		"category_id": {strconv.FormatInt(categoryID, 10)},
		"start_price": {"100"},
		"ends_at":     {"2020-01-01T12:00"},
		"ends_at_tz":  {"UTC"},
	}
	resp = postLotForm(t, admin, server, "/admin/lots", pastForm)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	pastID, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(resp.Header.Get("Location"), "/admin/lots/"), "/edit"), 10, 64)
	require.NoError(t, err)

	token := pageCSRFToken(t, admin, server.URL+"/admin/lots")
	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(pastID, 10)+"/publish", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, resp.Body)
	assert.Contains(t, resp.Body, "будущем")
	stored, ok = loadRawLot(t, ctx, pool, pastID)
	require.True(t, ok)
	assert.Equal(t, lot.StatusDraft, stored.status, "the past-deadline lot stays a draft")

	// Without a CSRF token the publication never happens (the middleware
	// rejects the mutation before any handler work).
	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/publish", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Publication opens the bidding.
	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/publish", url.Values{
		"gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/lots", resp.Header.Get("Location"))

	stored, ok = loadRawLot(t, ctx, pool, lotID)
	require.True(t, ok)
	assert.Equal(t, lot.StatusActive, stored.status)
	publishedEndsAt := stored.endsAt

	// The list shows the running auction and the remaining draft.
	resp = getRequest(t, admin, server.URL+"/admin/lots")
	assert.Contains(t, resp.Body, "торги идут")
	assert.Contains(t, resp.Body, "черновик")

	// After publication the conditions are immutable: every mutating request
	// is refused as a conflict and the stored row is exactly the same.
	resp = postLotForm(t, admin, server, "/admin/lots/"+strconv.FormatInt(lotID, 10), url.Values{
		"title": {"Взлом"}, "description": {"Взлом"}, "category_id": {"1"},
		"start_price": {"1"}, "ends_at": {"2030-01-01T00:00"},
	})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "черновик", "the refusal explains the draft-only rule")

	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/publish", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "republication is a conflict")
	assert.Contains(t, resp.Body, "черновик")

	stored, ok = loadRawLot(t, ctx, pool, lotID)
	require.True(t, ok)
	assert.Equal(t, "Серебряный рубль, грейд MS62", stored.title, "the hack did not change the title")
	assert.Equal(t, "Монета в слабе", stored.description)
	assert.Equal(t, int64(7500), stored.startPrice)
	assert.Equal(t, lot.StatusActive, stored.status)
	assert.True(t, publishedEndsAt.Equal(stored.endsAt), "republication and conflicts never move the deadline")

	resp = getRequest(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(lotID, 10)+"/edit")
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "the edit form is not offered for a published lot")

	// Deleting a draft removes it for good.
	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(pastID, 10)+"/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, ok = loadRawLot(t, ctx, pool, pastID)
	assert.False(t, ok, "the deleted draft is gone")

	resp = postForm(t, admin, server.URL+"/admin/lots/"+strconv.FormatInt(pastID, 10)+"/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "deleting a missing lot is 404")

	// Hostile strings are text, not markup.
	hostile := url.Values{
		"title":       {"<b>Марка</b> & \"книги\""},
		"description": {"Описание"},
		"category_id": {strconv.FormatInt(categoryID, 10)},
		"start_price": {"100"},
		"ends_at":     {"2026-10-05T12:00"},
		"ends_at_tz":  {"UTC"},
	}
	resp = postLotForm(t, admin, server, "/admin/lots", hostile)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/lots")
	assert.Contains(t, resp.Body, "&lt;b&gt;Марка&lt;/b&gt;")
	assert.NotContains(t, resp.Body, "<b>Марка</b>")
}
