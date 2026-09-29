//go:build integration

package seed

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/Lexv0lk/auction/internal/testutil"
)

const (
	adminPassword       = "demo-admin-pass"
	participantPassword = "demo-participant-pass"
)

func demoOptions() Options {
	return Options{AdminPassword: adminPassword, ParticipantPassword: participantPassword}
}

func TestSeedFillsEmptyDatabase(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	summary, err := Run(ctx, pool, demoOptions())
	require.NoError(t, err)

	assert.Equal(t, Summary{
		AdminCreated:        1,
		ParticipantsCreated: 3,
		CategoriesCreated:   4,
		LotsCreated:         4,
	}, summary)

	users := readUsers(t, ctx, pool)
	require.Len(t, users, 4, "one admin and three participants")
	assert.Equal(t, userRow{login: "demo-admin", role: "admin"}, userRow{login: users[0].login, role: users[0].role})

	hashes := make(map[string]string, len(users))
	for _, user := range users {
		assert.True(t, len(user.passwordHash) > 0, "user %s must store a hash", user.login)
		hashes[user.login] = user.passwordHash
	}
	assert.Len(t, hashes, 4, "stored hashes of equal passwords must still differ (random salt)")

	// The seed hashes must be accepted by the login verification itself.
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hashes["demo-admin"]), []byte(adminPassword)))
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hashes["demo-admin"]), []byte(participantPassword)),
		"the admin hash must reject a wrong password")
	for _, login := range []string{"demo-participant-1", "demo-participant-2", "demo-participant-3"} {
		require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hashes[login]), []byte(participantPassword)))
		assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hashes[login]), []byte(adminPassword)))
	}

	names := readCategoryNames(t, ctx, pool)
	assert.ElementsMatch(t,
		[]string{"Нумизматика", "Филателия", "Антикварные книги", "Живопись и графика"}, names,
		"the four demo categories must exist")

	lots := readLots(t, ctx, pool)
	require.Len(t, lots, 4, "draft examples with category relations")
	for _, lot := range lots {
		assert.Equal(t, "draft", lot.status)
		assert.Contains(t, names, lot.categoryName, "lot %s must reference a demo category", lot.title)
		assert.Greater(t, lot.startPrice, int64(0))

		minDeadline := time.Now().Add(time.Duration(lot.daysAhead-1) * 24 * time.Hour)
		assert.True(t, lot.endsAt.After(minDeadline),
			"lot %s deadline must stay ahead by about %d days", lot.title, lot.daysAhead)
	}
}

func TestSeedIsIdempotentOnRepeatedRun(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	first, err := Run(ctx, pool, demoOptions())
	require.NoError(t, err)
	require.Equal(t, 12, first.AdminCreated+first.ParticipantsCreated+first.CategoriesCreated+first.LotsCreated)

	snapshot := readEverything(t, ctx, pool)

	second, err := Run(ctx, pool, demoOptions())
	require.NoError(t, err)

	assert.Equal(t, Summary{
		AdminSkipped:        1,
		ParticipantsSkipped: 3,
		CategoriesSkipped:   4,
		LotsSkipped:         4,
	}, second, "a repeated run must only skip")
	assert.Equal(t, snapshot, readEverything(t, ctx, pool),
		"a repeated run must not change hashes, roles, fields or deadlines")
}

func TestSeedRollsBackPartialFilling(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	for _, stage := range []string{stageUsers, stageCategories, stageLots} {
		testutil.Reset(t, pool)
		mustExec(t, ctx, pool,
			"INSERT INTO users (login, password_hash, role) VALUES ('manual-user', 'hash', 'participant')")

		options := demoOptions()
		options.failAfterStage = stage

		_, err := Run(ctx, pool, options)
		require.ErrorIs(t, err, errInjected, "stage %s", stage)

		var users, categories, lots int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users))
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&categories))
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots").Scan(&lots))
		assert.Equal(t, 1, users, "stage %s: only the pre-existing account must remain", stage)
		assert.Zero(t, categories, "stage %s: categories must be rolled back", stage)
		assert.Zero(t, lots, "stage %s: lots must be rolled back", stage)

		var role string
		require.NoError(t, pool.QueryRow(ctx, "SELECT role FROM users WHERE login = 'manual-user'").Scan(&role))
		assert.Equal(t, "participant", role, "stage %s: pre-existing data must survive the rollback", stage)
	}
}

func TestSeedLeavesForeignRoleLoginUnchanged(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := testCtx(t)

	plantedHash, err := bcrypt.GenerateFromPassword([]byte("planted"), bcrypt.MinCost)
	require.NoError(t, err)
	mustExec(t, ctx, pool,
		"INSERT INTO users (login, password_hash, role) VALUES ('demo-admin', $1, 'participant')", string(plantedHash))

	_, err = Run(ctx, pool, demoOptions())
	require.ErrorIs(t, err, ErrLoginRoleConflict)
	assert.Contains(t, err.Error(), "demo-admin", "the conflict must name the login")
	assert.NotContains(t, err.Error(), adminPassword, "no password may appear in the message")

	var storedHash, storedRole string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT password_hash, role FROM users WHERE login = 'demo-admin'").Scan(&storedHash, &storedRole))
	assert.Equal(t, string(plantedHash), storedHash, "the existing account must keep its hash")
	assert.Equal(t, "participant", storedRole, "the existing account must keep its role")

	var users, categories, lots int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users))
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM categories").Scan(&categories))
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM lots").Scan(&lots))
	assert.Equal(t, 1, users, "the conflicting run must change nothing")
	assert.Zero(t, categories)
	assert.Zero(t, lots)
}

type userRow struct {
	login        string
	passwordHash string
	role         string
}

func readUsers(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []userRow {
	t.Helper()

	rows, err := pool.Query(ctx, "SELECT login, password_hash, role FROM users ORDER BY login")
	require.NoError(t, err)
	defer rows.Close()

	users := []userRow{}
	for rows.Next() {
		var user userRow
		require.NoError(t, rows.Scan(&user.login, &user.passwordHash, &user.role))
		users = append(users, user)
	}
	require.NoError(t, rows.Err())

	return users
}

func readCategoryNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()

	rows, err := pool.Query(ctx, "SELECT name FROM categories ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())

	return names
}

type lotRow struct {
	title        string
	description  string
	categoryName string
	startPrice   int64
	status       string
	endsAt       time.Time
	daysAhead    int
}

func readLots(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []lotRow {
	t.Helper()

	rows, err := pool.Query(ctx, "SELECT l.title, l.description, c.name, l.start_price, l.status, l.ends_at FROM lots l"+
		" JOIN categories c ON c.id = l.category_id ORDER BY l.id")
	require.NoError(t, err)
	defer rows.Close()

	lots := []lotRow{}
	for rows.Next() {
		var lot lotRow
		require.NoError(t, rows.Scan(&lot.title, &lot.description, &lot.categoryName, &lot.startPrice, &lot.status, &lot.endsAt))
		lots = append(lots, lot)
	}
	require.NoError(t, rows.Err())

	// The expected margins live in the seed definitions; titles are unique.
	expectedDays := map[string]int{
		"Серебряный рубль 1726 года":                  7,
		"Земская почтовая марка, 1889 год":            10,
		"Иллюстрированный каталог выставки, 1903 год": 14,
		"Акварель с видом на залив, начало XX века":   12,
	}
	for i := range lots {
		lots[i].daysAhead = expectedDays[lots[i].title]
	}

	return lots
}

type everything struct {
	users      []userRow
	categories []string
	lots       []lotRow
}

// readEverything captures the full demo state; a repeated seed run must not
// change any of it, including the stored deadline timestamps.
func readEverything(t *testing.T, ctx context.Context, pool *pgxpool.Pool) everything {
	t.Helper()

	lots := readLots(t, ctx, pool)
	for i := range lots {
		lots[i].daysAhead = 0
	}

	return everything{
		users:      readUsers(t, ctx, pool),
		categories: readCategoryNames(t, ctx, pool),
		lots:       lots,
	}
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	_, err := pool.Exec(ctx, sql, args...)
	require.NoError(t, err, "statement %q", sql)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}
