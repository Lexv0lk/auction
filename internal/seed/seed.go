// Package seed fills a migrated database with the demo data set of the
// educational auction: one administrator, three participants, artifact
// categories and draft lots. Run is a one-off administrative operation: it is
// transactional, idempotent on stable keys and never resets or changes an
// already existing record, so repeating the command keeps the current state.
package seed

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Lexv0lk/auction/internal/password"
	"github.com/Lexv0lk/auction/internal/postgres"
)

// Fixed demo logins are the stable keys of the demo accounts; they are always
// inserted already normalized (lowercase, no surrounding spaces).
const (
	adminLogin = "demo-admin"

	adminRole       = "admin"
	participantRole = "participant"
)

var demoParticipantLogins = []string{
	"demo-participant-1",
	"demo-participant-2",
	"demo-participant-3",
}

// Categories are recognized by their normalized name, the same key the
// categories_name_lower_idx index enforces.
var demoCategories = []string{
	"Нумизматика",
	"Филателия",
	"Антикварные книги",
	"Живопись и графика",
}

// demoLot is a draft example; the stable key of a demo lot is its title
// together with the category, so a repeated run finds and skips it.
type demoLot struct {
	title       string
	description string
	category    string
	startPrice  int64
	daysAhead   int
}

// Deadlines come from the database clock (now() + make_interval) with a margin
// for a manual demonstration; the seed never shifts the deadline of an
// existing lot on a repeated run.
var demoLots = []demoLot{
	{
		title:       "Серебряный рубль 1726 года",
		description: "Серебряная монета в хорошем состоянии, видны следы бытования. Стартовая цена указана в условных единицах учебного аукциона.",
		category:    "Нумизматика",
		startPrice:  5000,
		daysAhead:   7,
	},
	{
		title:       "Земская почтовая марка, 1889 год",
		description: "Почтовая марка без клея, с зубцовым дефектом. Учебный пример черновика для проверки каталога и ставок.",
		category:    "Филателия",
		startPrice:  1200,
		daysAhead:   10,
	},
	{
		title:       "Иллюстрированный каталог выставки, 1903 год",
		description: "Старинный каталог с гравюрами, переплёт потёрт. Пример лота с более длинным дедлайном для ручной проверки.",
		category:    "Антикварные книги",
		startPrice:  3000,
		daysAhead:   14,
	},
	{
		title:       "Акварель с видом на залив, начало XX века",
		description: "Акварель неизвестного художника на бумаге, без рамы. Учебный пример для демонстрации ставок участников.",
		category:    "Живопись и графика",
		startPrice:  2500,
		daysAhead:   12,
	},
}

// Stages of Run used by the failure-injection test hook.
const (
	stageUsers      = "users"
	stageCategories = "categories"
	stageLots       = "lots"
)

var (
	// ErrLoginRoleConflict means a fixed demo login is already used by an
	// account with a different role; the seed never changes such an account.
	ErrLoginRoleConflict = errors.New("demo login belongs to an account with another role")

	errAdminPasswordMissing       = errors.New("SEED_ADMIN_PASSWORD is required; see README.md for the demo set")
	errParticipantPasswordMissing = errors.New("SEED_PARTICIPANT_PASSWORD is required; see README.md for the demo set")
	errDemoCategoryNotSeeded      = errors.New("demo category is missing after seeding")
	errInjected                   = errors.New("seed: injected failure")
)

// Pool is the subset of the connection pool the seed needs: the schema version
// check and a single transaction.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Options carries the passwords of the demo accounts, taken from the
// environment by the seed command.
type Options struct {
	AdminPassword       string
	ParticipantPassword string

	// failAfterStage aborts the run after the named stage for rollback tests.
	failAfterStage string
}

// Summary reports how many objects were created and skipped during a run.
type Summary struct {
	AdminCreated        int
	AdminSkipped        int
	ParticipantsCreated int
	ParticipantsSkipped int
	CategoriesCreated   int
	CategoriesSkipped   int
	LotsCreated         int
	LotsSkipped         int
}

func (s Summary) String() string {
	return fmt.Sprintf(
		"seed complete: admin created=%d skipped=%d, participants created=%d skipped=%d, "+
			"categories created=%d skipped=%d, lots created=%d skipped=%d",
		s.AdminCreated, s.AdminSkipped, s.ParticipantsCreated, s.ParticipantsSkipped,
		s.CategoriesCreated, s.CategoriesSkipped, s.LotsCreated, s.LotsSkipped)
}

type demoUser struct {
	login        string
	role         string
	passwordHash string
}

const (
	selectUserByLoginSQL = "SELECT role FROM users WHERE login = $1"
	insertUserSQL        = "INSERT INTO users (login, password_hash, role) VALUES ($1, $2, $3)"
	selectCategoryIdSQL  = "SELECT id FROM categories WHERE lower(name) = lower($1)"
	insertCategorySQL    = "INSERT INTO categories (name) VALUES ($1) RETURNING id"
	selectLotByKeySQL    = "SELECT id FROM lots WHERE lower(title) = lower($1) AND category_id = $2"
	insertLotSQL         = "INSERT INTO lots (title, description, category_id, start_price, status, ends_at)" +
		" VALUES ($1, $2, $3, $4, 'draft', now() + make_interval(days => $5::int)) RETURNING id"
)

// Run fills the database with the demo data set inside a single transaction
// and returns the counts of created and skipped objects. An existing record
// with the same stable key is left untouched; an existing demo login with a
// foreign role aborts the run without changing that account.
func Run(ctx context.Context, pool Pool, options Options) (Summary, error) {
	if err := options.validate(); err != nil {
		return Summary{}, err
	}

	users, err := buildDemoUsers(options)
	if err != nil {
		return Summary{}, err
	}

	if err := postgres.CheckSchemaVersion(ctx, pool); err != nil {
		return Summary{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summary := Summary{}
	if err := seedUsers(ctx, tx, users, &summary); err != nil {
		return Summary{}, err
	}
	if err := options.failAfter(stageUsers); err != nil {
		return Summary{}, err
	}

	categoryIDs, err := seedCategories(ctx, tx, &summary)
	if err != nil {
		return Summary{}, err
	}
	if err := options.failAfter(stageCategories); err != nil {
		return Summary{}, err
	}

	if err := seedLots(ctx, tx, categoryIDs, &summary); err != nil {
		return Summary{}, err
	}
	if err := options.failAfter(stageLots); err != nil {
		return Summary{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Summary{}, fmt.Errorf("commit seed transaction: %w", err)
	}

	return summary, nil
}

func (o Options) validate() error {
	if o.AdminPassword == "" {
		return errAdminPasswordMissing
	}
	if o.ParticipantPassword == "" {
		return errParticipantPasswordMissing
	}

	return nil
}

func (o Options) failAfter(stage string) error {
	if o.failAfterStage == stage {
		return errInjected
	}

	return nil
}

// buildDemoUsers hashes the passwords before the transaction starts. Every
// participant gets its own hash, so the shared demo password is still salted
// separately for each stored hash.
func buildDemoUsers(options Options) ([]demoUser, error) {
	adminHash, err := password.Hash(options.AdminPassword)
	if err != nil {
		return nil, fmt.Errorf("hash password for %s: %w", adminLogin, err)
	}

	users := make([]demoUser, 0, 1+len(demoParticipantLogins))
	users = append(users, demoUser{login: adminLogin, role: adminRole, passwordHash: adminHash})

	for _, login := range demoParticipantLogins {
		hash, err := password.Hash(options.ParticipantPassword)
		if err != nil {
			return nil, fmt.Errorf("hash password for %s: %w", login, err)
		}

		users = append(users, demoUser{login: login, role: participantRole, passwordHash: hash})
	}

	return users, nil
}

func seedUsers(ctx context.Context, tx pgx.Tx, users []demoUser, summary *Summary) error {
	for _, user := range users {
		var role string
		err := tx.QueryRow(ctx, selectUserByLoginSQL, user.login).Scan(&role)
		if err == nil {
			if role != user.role {
				return fmt.Errorf("%w: login %q already belongs to a %q account, expected %q; "+
					"the existing account is left unchanged", ErrLoginRoleConflict, user.login, role, user.role)
			}

			summary.skipUser(user.role)

			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up demo user %s: %w", user.login, err)
		}

		if _, err := tx.Exec(ctx, insertUserSQL, user.login, user.passwordHash, user.role); err != nil {
			return fmt.Errorf("create demo user %s: %w", user.login, err)
		}

		summary.addUser(user.role)
	}

	return nil
}

func seedCategories(ctx context.Context, tx pgx.Tx, summary *Summary) (map[string]int64, error) {
	categoryIDs := make(map[string]int64, len(demoCategories))
	for _, name := range demoCategories {
		var id int64
		err := tx.QueryRow(ctx, selectCategoryIdSQL, name).Scan(&id)
		if err == nil {
			categoryIDs[name] = id
			summary.CategoriesSkipped++

			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("look up demo category %s: %w", name, err)
		}

		if err := tx.QueryRow(ctx, insertCategorySQL, name).Scan(&id); err != nil {
			return nil, fmt.Errorf("create demo category %s: %w", name, err)
		}
		categoryIDs[name] = id
		summary.CategoriesCreated++
	}

	return categoryIDs, nil
}

func seedLots(ctx context.Context, tx pgx.Tx, categoryIDs map[string]int64, summary *Summary) error {
	for _, lot := range demoLots {
		categoryID, ok := categoryIDs[lot.category]
		if !ok {
			return fmt.Errorf("%w: %s", errDemoCategoryNotSeeded, lot.category)
		}

		var id int64
		err := tx.QueryRow(ctx, selectLotByKeySQL, lot.title, categoryID).Scan(&id)
		if err == nil {
			summary.LotsSkipped++

			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up demo lot %s: %w", lot.title, err)
		}

		if err := tx.QueryRow(ctx, insertLotSQL, lot.title, lot.description, categoryID, lot.startPrice, lot.daysAhead).
			Scan(&id); err != nil {
			return fmt.Errorf("create demo lot %s: %w", lot.title, err)
		}

		summary.LotsCreated++
	}

	return nil
}

func (s *Summary) addUser(role string) {
	if role == adminRole {
		s.AdminCreated++

		return
	}
	s.ParticipantsCreated++
}

func (s *Summary) skipUser(role string) {
	if role == adminRole {
		s.AdminSkipped++

		return
	}
	s.ParticipantsSkipped++
}
