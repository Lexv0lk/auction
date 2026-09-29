package httpapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Lexv0lk/auction/internal/auth"
	"github.com/Lexv0lk/auction/internal/category"
)

var errCategoriesDown = errors.New("categories database down")

// loggedInClient returns a server whose jar already holds a session of the
// given account role.
func loggedInClient(t *testing.T, user auth.User, deps ...any) (*httptest.Server, *http.Client) {
	t.Helper()

	sessionToken := strings.Repeat("f", 64)
	authenticator := &fakeAuthenticator{
		userFunc: func(_ context.Context, token string) (auth.User, error) {
			if token == sessionToken {
				return user, nil
			}

			return auth.User{}, auth.ErrNoSession
		},
	}
	server, client := newTestServer(t, authenticator, testConfig(), deps...)
	setSessionCookie(t, client, server.URL, sessionToken)

	return server, client
}

func adminClient(t *testing.T, deps ...any) (*httptest.Server, *http.Client) {
	t.Helper()

	return loggedInClient(t, auth.User{ID: 1, Login: "demo-admin", Role: auth.RoleAdmin}, deps...)
}

func pageCSRFToken(t *testing.T, client *http.Client, target string) string {
	t.Helper()

	resp := getRequest(t, client, target)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	match := csrfFieldPattern.FindStringSubmatch(resp.Body)
	require.NotNil(t, match, "the page must carry a CSRF field")

	return match[1]
}

func TestAdminCategoryPagesRequireAdmin(t *testing.T) {
	server, client := newTestServer(t, &fakeAuthenticator{}, testConfig())

	// A guest is redirected to the login page, not answered with an error.
	resp := getRequest(t, client, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))

	participant := auth.User{ID: 2, Login: "demo-participant-1", Role: auth.RoleParticipant}
	server, client = loggedInClient(t, participant, &fakeCategories{})
	resp = getRequest(t, client, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, resp.Body, "Недостаточно прав")

	resp = getRequest(t, client, server.URL+"/admin/categories/1/edit")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	token := pageCSRFToken(t, client, server.URL+"/login")
	resp = postForm(t, client, server.URL+"/admin/categories", url.Values{
		"name": {"Боны"}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a participant must not create categories")
}

func TestCategoriesPageListsCategoriesWithForms(t *testing.T) {
	categories := &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return []category.Category{
				{ID: 2, Name: "Антикварные книги"},
				{ID: 1, Name: "Нумизматика"},
			}, nil
		},
	}
	server, client := adminClient(t, categories)

	resp := getRequest(t, client, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "Антикварные книги")
	assert.Contains(t, resp.Body, "Нумизматика")
	assert.Contains(t, resp.Body, `action="/admin/categories"`, "the create form is on the list page")
	assert.Contains(t, resp.Body, `action="/admin/categories/2/delete"`, "each row carries its delete action")
	assert.Contains(t, resp.Body, `/admin/categories/1/edit`, "each row links to its edit form")
	require.Equal(t, 1, categories.listCalls, "the page loads the list once")
}

func TestCategoryCreateRedirectsToTheList(t *testing.T) {
	var capturedName string
	categories := &fakeCategories{
		createFunc: func(_ context.Context, name string) (category.Category, error) {
			capturedName = name

			return category.Category{ID: 7, Name: "Монеты"}, nil
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories", url.Values{
		"name": {"  Монеты "}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/categories", resp.Header.Get("Location"), "a redirect keeps a page reload from resubmitting")
	assert.Equal(t, "  Монеты ", capturedName, "the handler passes the raw input through; the service normalizes it")
}

func TestCategoryCreateShowsFieldErrorsAndPreservesInput(t *testing.T) {
	for _, tc := range []struct {
		name          string
		input         string
		serviceErr    error
		status        int
		wantSubstring string
	}{
		{"empty name", "   ", category.ErrNameEmpty, http.StatusUnprocessableEntity, "название"},
		{"too long name", strings.Repeat("а", category.MaxNameLength+1), category.ErrNameTooLong, http.StatusUnprocessableEntity, "длиннее"},
		{"duplicate name", "Нумизматика", category.ErrExists, http.StatusConflict, "уже существует"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			categories := &fakeCategories{
				listFunc: func(context.Context) ([]category.Category, error) {
					return []category.Category{{ID: 1, Name: "Нумизматика"}}, nil
				},
				createFunc: func(context.Context, string) (category.Category, error) {
					return category.Category{}, tc.serviceErr
				},
			}
			server, client := adminClient(t, categories)

			token := pageCSRFToken(t, client, server.URL+"/admin/categories")
			resp := postForm(t, client, server.URL+"/admin/categories", url.Values{
				"name": {tc.input}, "gorilla.csrf.Token": {token},
			})
			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Contains(t, resp.Body, tc.wantSubstring)
			assert.Contains(t, resp.Body, `name="name" value="`, "the form keeps the entered name")
			assert.Contains(t, resp.Body, "Нумизматика", "the failure answer still shows the current list")
			assert.NotContains(t, resp.Body, "Категорий пока нет")

			trimmed := strings.TrimSpace(tc.input)
			if trimmed != "" && len(trimmed) <= 200 {
				assert.Contains(t, resp.Body, `value="`+trimmed+`"`, "the preserved input is the submitted value")
			}
			assert.Nil(t, sessionCookie(resp), "a failed create must not touch the session")
		})
	}
}

func TestCategoryCreateDatabaseFailureIs503(t *testing.T) {
	categories := &fakeCategories{
		createFunc: func(context.Context, string) (category.Category, error) {
			return category.Category{}, errCategoriesDown
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories", url.Values{
		"name": {"Монеты"}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, resp.Body, "временно недоступен")
	assert.NotContains(t, resp.Body, "categories database down", "internal details must not leak")
}

func TestCategoryEditPageShowsCurrentName(t *testing.T) {
	categories := &fakeCategories{
		getFunc: func(_ context.Context, id int64) (category.Category, error) {
			require.Equal(t, int64(3), id)

			return category.Category{ID: 3, Name: "Филателия"}, nil
		},
	}
	server, client := adminClient(t, categories)

	resp := getRequest(t, client, server.URL+"/admin/categories/3/edit")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `action="/admin/categories/3"`, "the form posts to the rename action")
	assert.Contains(t, resp.Body, `value="Филателия"`)
}

func TestCategoryEditUnknownOrMalformedIDIs404(t *testing.T) {
	categories := &fakeCategories{
		getFunc: func(context.Context, int64) (category.Category, error) {
			return category.Category{}, category.ErrNotFound
		},
	}
	server, client := adminClient(t, categories)

	resp := getRequest(t, client, server.URL+"/admin/categories/99/edit")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Body, "не найдена")

	resp = getRequest(t, client, server.URL+"/admin/categories/not-a-number/edit")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A malformed ID never reaches the service.
	resp = getRequest(t, client, server.URL+"/admin/categories/99/delete")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestCategoryRenameFlow(t *testing.T) {
	categories := &fakeCategories{
		renameFunc: func(_ context.Context, id int64, name string) (category.Category, error) {
			require.Equal(t, int64(3), id)
			require.Equal(t, " Боны ", name, "the handler passes the raw input through; the service normalizes it")

			return category.Category{ID: 3, Name: "Боны"}, nil
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories/3/edit")
	resp := postForm(t, client, server.URL+"/admin/categories/3", url.Values{
		"name": {" Боны "}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/categories", resp.Header.Get("Location"))
}

func TestCategoryRenameFailuresKeepTheForm(t *testing.T) {
	for _, tc := range []struct {
		name          string
		serviceErr    error
		status        int
		wantSubstring string
	}{
		{"duplicate", category.ErrExists, http.StatusConflict, "уже существует"},
		{"empty", category.ErrNameEmpty, http.StatusUnprocessableEntity, "название"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			categories := &fakeCategories{
				renameFunc: func(context.Context, int64, string) (category.Category, error) {
					return category.Category{}, tc.serviceErr
				},
			}
			server, client := adminClient(t, categories)

			token := pageCSRFToken(t, client, server.URL+"/admin/categories/3/edit")
			resp := postForm(t, client, server.URL+"/admin/categories/3", url.Values{
				"name": {"Боны"}, "gorilla.csrf.Token": {token},
			})
			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Contains(t, resp.Body, tc.wantSubstring)
			assert.Contains(t, resp.Body, `value="Боны"`, "the failed rename keeps the entered name")
		})
	}

	categories := &fakeCategories{
		renameFunc: func(context.Context, int64, string) (category.Category, error) {
			return category.Category{}, category.ErrNotFound
		},
	}
	server, client := adminClient(t, categories)
	token := pageCSRFToken(t, client, server.URL+"/admin/categories/3/edit")
	resp := postForm(t, client, server.URL+"/admin/categories/3", url.Values{
		"name": {"Боны"}, "gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "renaming a deleted category is 404, not an error form")
}

func TestCategoryDeleteFlow(t *testing.T) {
	var deletedID int64
	categories := &fakeCategories{
		deleteFunc: func(_ context.Context, id int64) error {
			deletedID = id

			return nil
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories/5/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, int64(5), deletedID)
	assert.Equal(t, "/admin/categories", resp.Header.Get("Location"))
}

func TestCategoryDeleteInUseShowsConflict(t *testing.T) {
	categories := &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return []category.Category{{ID: 5, Name: "Нумизматика"}}, nil
		},
		deleteFunc: func(context.Context, int64) error {
			return category.ErrInUse
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories/5/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, resp.Body, "использу", "the conflict explains the lots referencing the category")
	assert.Contains(t, resp.Body, "Нумизматика", "the list stays available after the refusal")
	assert.Equal(t, 2, categories.listCalls, "the refusal re-renders the list")
}

func TestCategoryDeleteUnknownIs404(t *testing.T) {
	categories := &fakeCategories{
		deleteFunc: func(context.Context, int64) error {
			return category.ErrNotFound
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories/99/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestCategoryDeleteDatabaseFailureIs503(t *testing.T) {
	categories := &fakeCategories{
		deleteFunc: func(context.Context, int64) error {
			return errCategoriesDown
		},
	}
	server, client := adminClient(t, categories)

	token := pageCSRFToken(t, client, server.URL+"/admin/categories")
	resp := postForm(t, client, server.URL+"/admin/categories/5/delete", url.Values{
		"gorilla.csrf.Token": {token},
	})
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotContains(t, resp.Body, "categories database down")
}

func TestCategoriesPageDatabaseFailureIs503(t *testing.T) {
	categories := &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return nil, errCategoriesDown
		},
	}
	server, client := adminClient(t, categories)

	resp := getRequest(t, client, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotContains(t, resp.Body, "categories database down")
}

func TestCategoryFormsAreProtectedByCSRF(t *testing.T) {
	categories := &fakeCategories{}
	server, client := adminClient(t, categories)

	// Without a token the middleware rejects the mutation before any handler.
	resp := postForm(t, client, server.URL+"/admin/categories", url.Values{"name": {"Боны"}})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Zero(t, categories.createCalls, "no create without CSRF")

	resp = postForm(t, client, server.URL+"/admin/categories/5/delete", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Zero(t, categories.deleteCalls, "no delete without CSRF")
}

func TestCategoryNamesRenderEscaped(t *testing.T) {
	hostile := `<b>Боны</b> & "книги"`
	categories := &fakeCategories{
		listFunc: func(context.Context) ([]category.Category, error) {
			return []category.Category{{ID: 8, Name: hostile}}, nil
		},
		getFunc: func(context.Context, int64) (category.Category, error) {
			return category.Category{ID: 8, Name: hostile}, nil
		},
	}
	server, client := adminClient(t, categories)

	resp := getRequest(t, client, server.URL+"/admin/categories")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, "&lt;b&gt;Боны&lt;/b&gt;", "list names are text, not markup")
	assert.NotContains(t, resp.Body, "<b>Боны</b>")

	resp = getRequest(t, client, server.URL+"/admin/categories/8/edit")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, resp.Body, "<b>Боны</b>", "edit form values are escaped too")
}

func TestAdminNavigationLinksCategories(t *testing.T) {
	server, client := adminClient(t, &fakeCategories{})

	resp := getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Body, `href="/admin/categories"`, "the admin navigation shows the reference data")

	participant := auth.User{ID: 2, Login: "demo-participant-1", Role: auth.RoleParticipant}
	server, client = loggedInClient(t, participant, &fakeCategories{})
	resp = getRequest(t, client, server.URL+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, resp.Body, `href="/admin/categories"`, "participants see no admin navigation")
}
