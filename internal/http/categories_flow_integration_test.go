//go:build integration

package httpapp

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/testutil"
)

var editLinkPattern = regexp.MustCompile(`/admin/categories/(\d+)/edit`)

// loginClient signs in through the real login form and returns the client
// with its session.
func loginClient(t *testing.T, server *httptest.Server, login, plain string) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           jar,
	}
	token := loginPageToken(t, client, server)
	resp := postForm(t, client, server.URL+"/login", url.Values{
		"login": {login}, "password": {plain}, "gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "login of %s", login)

	return client
}

func createCategory(t *testing.T, client *http.Client, server *httptest.Server, name string) *httpResponse {
	t.Helper()

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")

	return postForm(t, client, server.URL+"/admin/categories", url.Values{
		"name": {name}, "gorilla.csrf.Token": {token},
	})
}

func deleteCategory(t *testing.T, client *http.Client, server *httptest.Server, id int64) *httpResponse {
	t.Helper()

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")

	return postForm(t, client, server.URL+"/admin/categories/"+strconv.FormatInt(id, 10)+"/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
}

// TestCategoriesAdminFlowAgainstPostgreSQL walks the admin reference-data
// lifecycle through the real services and templates.
func TestCategoriesAdminFlowAgainstPostgreSQL(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	hash, err := password.Hash("integration-pass")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO users (login, password_hash, role) VALUES ('flow-cat-admin', $1, 'admin')", hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO users (login, password_hash, role) VALUES ('flow-cat-participant', $1, 'participant')", hash)
	require.NoError(t, err)

	handler, err := NewHandler(discardLogger(), auth.NewService(pool), category.NewService(pool), testConfig())
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// Access control against the real database: a guest is redirected, a
	// participant is refused, an admin works with the reference data.
	guest, err := cookiejar.New(nil)
	require.NoError(t, err)
	guestClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Jar:           guest,
	}
	resp := getRequest(t, guestClient, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	participant := loginClient(t, server, "flow-cat-participant", "integration-pass")
	resp = getRequest(t, participant, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// The participant takes the CSRF token from a page it can see (the home
	// navigation) and still must not reach the reference data.
	home := getRequest(t, participant, server.URL+"/")
	require.Equal(t, http.StatusOK, home.StatusCode)
	match := csrfFieldPattern.FindStringSubmatch(home.Body)
	require.NotNil(t, match)
	resp = postForm(t, participant, server.URL+"/admin/categories", url.Values{
		"name": {"Шпионская категория"}, "gorilla.csrf.Token": {match[1]},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a participant must not create categories")

	var categories int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&categories))
	assert.Zero(t, categories, "refused requests never touch the reference data")

	admin := loginClient(t, server, "flow-cat-admin", "integration-pass")
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Категорий пока нет")

	// Create: trimming applies, the list shows the stored name.
	resp = createCategory(t, admin, server, "  Нумизматика ")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	assert.Contains(t, resp.Body, "Нумизматика")
	match = editLinkPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the created category appears with its edit link")
	categoryID := match[1]

	// Duplicates and invalid names come back as form answers.
	resp = createCategory(t, admin, server, "НУМИЗМАТИКА")
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "уже существует")
	resp = createCategory(t, admin, server, "   ")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, resp.Body, "Введите название")
	resp = createCategory(t, admin, server, strings.Repeat("а", category.MaxNameLength+1))
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	var stored int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&stored))
	assert.Equal(t, 1, stored, "failed creates leave no rows behind")

	// Edit: the form pre-fills the current name, the rename is applied.
	resp = getRequest(t, admin, server.URL+"/admin/categories/"+categoryID+"/edit")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `value="Нумизматика"`)
	token := pageCSRFToken(t, admin, server.URL+"/admin/categories/"+categoryID+"/edit")
	resp = postForm(t, admin, server.URL+"/admin/categories/"+categoryID, url.Values{
		"name": {"Монеты и боны"}, "gorilla.csrf.Token": {token},
	})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	assert.Contains(t, resp.Body, "Монеты и боны")
	assert.NotContains(t, resp.Body, ">Нумизматика<")

	// Delete of an unused category removes it for good.
	categoryNum, err := strconv.ParseInt(categoryID, 10, 64)
	require.NoError(t, err)
	resp = deleteCategory(t, admin, server, categoryNum)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&stored))
	assert.Zero(t, stored)

	// Missing IDs are 404 in both directions.
	resp = getRequest(t, admin, server.URL+"/admin/categories/999/edit")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories/abc/edit")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp = deleteCategory(t, admin, server, 999)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A category referenced by a lot (fixture until step 07) is protected.
	resp = createCategory(t, admin, server, "Занятая категория")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	match = editLinkPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match)
	usedID, err := strconv.ParseInt(match[1], 10, 64)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		"INSERT INTO lots (title, description, category_id, start_price, status, ends_at)"+
			" VALUES ('Марка', 'Учебный лот', $1, 100, 'draft', now() + make_interval(days => 7))", usedID)
	require.NoError(t, err)

	resp = deleteCategory(t, admin, server, usedID)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "использу")
	assert.Contains(t, resp.Body, "Занятая категория", "the list stays after the refusal")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&stored))
	assert.Equal(t, 1, stored, "the used category stays")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots").Scan(&stored))
	assert.Equal(t, 1, stored, "the lot stays")

	// Hostile names are text in the list, not markup.
	resp = createCategory(t, admin, server, "<i>Монеты</i>")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp = getRequest(t, admin, server.URL+"/admin/categories")
	assert.Contains(t, resp.Body, "&lt;i&gt;Монеты&lt;/i&gt;")
	assert.NotContains(t, resp.Body, "<i>Монеты</i>")
}
