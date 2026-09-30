// Command verify checks the database state a load run must leave behind.
// It runs separately from the load tool and from the application: the k6
// scenarios of load/ produce the traffic, this command proves the data
// correctness afterwards — the finished lot, the stored winner and the exact
// reconciliation between confirmed bid operations and stored rows.
//
// Configuration comes from the environment (see docs/load-testing.md):
//
//	DATABASE_URL          PostgreSQL connection string of the loaded database
//	VERIFY_MODE           "bids" (one competed lot) or "catalog" (read-only lots)
//	VERIFY_LOT_TITLE      exact lot title for VERIFY_MODE=bids
//	VERIFY_TITLE_PREFIX   title prefix for VERIFY_MODE=catalog
//	VERIFY_EXPECTED_NEW   confirmed new bids counted by the k6 report (bids mode)
//	VERIFY_UNCERTAIN      number of commit_outcome_unknown attempts (bids mode)
//	VERIFY_EXPECTED_LOTS  prepared lot count to compare against (catalog mode; 0 skips)
//	VERIFY_WAIT           how long to wait for the worker to finish the lot (bids mode)
//	VERIFY_OUTPUT         optional path for the machine-readable JSON report
//
// The command prints the checks and exits non-zero when any check fails.
// Passwords and connection strings never enter the output.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lexv0lk/auction/internal/config"
	"github.com/Lexv0lk/auction/internal/postgres"
)

// Modes select which fact set the database is queried for.
const (
	modeBids    = "bids"
	modeCatalog = "catalog"
)

var (
	errDatabaseURLRequired = errors.New("DATABASE_URL is required")
	errModeRequired        = errors.New("VERIFY_MODE must be bids or catalog")
	errLotTitleRequired    = errors.New("VERIFY_LOT_TITLE is required for VERIFY_MODE=bids")
	errTitlePrefixRequired = errors.New("VERIFY_TITLE_PREFIX is required for VERIFY_MODE=catalog")
	errLotNotFound         = errors.New("lot with the given title was not found")
	errChecksFailed        = errors.New("verification failed")
)

// check is one named correctness decision of the report.
type check struct {
	Name      string `json:"name"`
	Passed    bool   `json:"passed"`
	Detail    string `json:"detail,omitempty"`
	DetailInt int64  `json:"detail_int,omitempty"`
}

// bidFacts carries everything the bid-mode checks need; the unit tests feed
// mutated copies of it, so every failure mode stays testable without a DB.
type bidFacts struct {
	Finished           bool
	FinishDelaySeconds float64
	BidsCount          int64
	MaxAmount          int64
	MaxAmountBidID     int64
	HasWinningBid      bool
	WinningBidID       int64
	WinningAmount      int64
	DecreaseViolations int64
	DistinctUserKeys   int64
	ExpectedNew        int64
	UncertainOutcomes  int64
}

// catalogFacts carries the read-scenario facts: the prepared lots must still
// be untouched by any bid.
type catalogFacts struct {
	LotsCount    int64
	ActiveCount  int64
	BidsCount    int64
	ExpectedLots int64
}

// withinUncertainty reports whether the stored row count reconciles with the
// client-confirmed count: every 503 commit_outcome_unknown attempt may or may
// not have stored a row, so the truth lies in [expected, expected+uncertain].
func withinUncertainty(expected, uncertain, actual int64) bool {
	if expected < 0 || uncertain < 0 || actual < expected || actual > expected+uncertain {
		return false
	}

	return true
}

// bidChecks names the correctness decisions of one competed lot. Every check
// explains itself, so a failing report is readable without re-running the load.
func bidChecks(f bidFacts) []check {
	checks := make([]check, 0, 7)
	finished := check{Name: "worker_finished", Passed: f.Finished}
	if f.Finished {
		finished.Detail = strconv.FormatFloat(f.FinishDelaySeconds, 'f', 2, 64)
	} else {
		finished.Detail = "the lot was not finished within the verification wait"
	}
	checks = append(checks, finished)

	checks = append(checks, check{
		Name:      "bids_present",
		Passed:    f.BidsCount > 0,
		Detail:    fmt.Sprintf("stored=%d", f.BidsCount),
		DetailInt: f.BidsCount,
	})
	checks = append(checks, check{
		Name:   "count_matches_confirmed",
		Passed: withinUncertainty(f.ExpectedNew, f.UncertainOutcomes, f.BidsCount),
		Detail: fmt.Sprintf("stored=%d confirmed_new=%d uncertain=%d", f.BidsCount, f.ExpectedNew, f.UncertainOutcomes),
	})
	checks = append(checks, check{
		Name:      "request_keys_unique",
		Passed:    f.DistinctUserKeys == f.BidsCount,
		Detail:    fmt.Sprintf("distinct=%d stored=%d", f.DistinctUserKeys, f.BidsCount),
		DetailInt: f.DistinctUserKeys,
	})
	checks = append(checks, check{
		Name:      "amounts_strictly_increase",
		Passed:    f.DecreaseViolations == 0,
		Detail:    fmt.Sprintf("violations=%d", f.DecreaseViolations),
		DetailInt: f.DecreaseViolations,
	})
	checks = append(checks, check{
		Name:   "winner_is_maximal_bid",
		Passed: f.HasWinningBid && f.WinningBidID == f.MaxAmountBidID,
		Detail: fmt.Sprintf("winning_bid_id=%d maximal_bid_id=%d", f.WinningBidID, f.MaxAmountBidID),
	})
	checks = append(checks, check{
		Name:   "winner_amount_matches_max",
		Passed: f.HasWinningBid && f.WinningAmount == f.MaxAmount,
		Detail: fmt.Sprintf("winning_amount=%d max_amount=%d", f.WinningAmount, f.MaxAmount),
	})

	return checks
}

// catalogChecks names the correctness decisions of one read-only run: the
// prepared lots stay published and active, and no read has bid anything.
func catalogChecks(f catalogFacts) []check {
	checks := make([]check, 0, 3)
	prepared := check{Name: "lots_prepared", DetailInt: f.LotsCount}
	switch {
	case f.ExpectedLots > 0:
		prepared.Passed = f.LotsCount == f.ExpectedLots
		prepared.Detail = fmt.Sprintf("found=%d expected=%d", f.LotsCount, f.ExpectedLots)
	default:
		prepared.Passed = true
		prepared.Detail = fmt.Sprintf("found=%d, expectation skipped (VERIFY_EXPECTED_LOTS unset)", f.LotsCount)
	}
	checks = append(checks, prepared)
	checks = append(checks, check{
		Name:   "lots_stay_active",
		Passed: f.ActiveCount == f.LotsCount,
		Detail: fmt.Sprintf("active=%d total=%d", f.ActiveCount, f.LotsCount),
	})
	checks = append(checks, check{
		Name:      "reads_did_not_bid",
		Passed:    f.BidsCount == 0,
		Detail:    fmt.Sprintf("bids=%d", f.BidsCount),
		DetailInt: f.BidsCount,
	})

	return checks
}

func allPassed(checks []check) bool {
	for _, c := range checks {
		if !c.Passed {
			return false
		}
	}

	return true
}

type settings struct {
	databaseURL  string
	mode         string
	lotTitle     string
	titlePrefix  string
	expectedNew  int64
	uncertain    int64
	expectedLots int64
	wait         time.Duration
	output       string
}

func loadSettings(environ func(string) string) (settings, error) {
	s := settings{
		databaseURL: environ("DATABASE_URL"),
		mode:        environ("VERIFY_MODE"),
		lotTitle:    environ("VERIFY_LOT_TITLE"),
		titlePrefix: environ("VERIFY_TITLE_PREFIX"),
		output:      environ("VERIFY_OUTPUT"),
		wait:        180 * time.Second,
	}
	if s.databaseURL == "" {
		return s, errDatabaseURLRequired
	}
	if s.mode != modeBids && s.mode != modeCatalog {
		return s, errModeRequired
	}
	if raw := environ("VERIFY_EXPECTED_NEW"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return s, fmt.Errorf("VERIFY_EXPECTED_NEW %w", errInvalidPositiveInteger)
		}
		s.expectedNew = value
	}
	if raw := environ("VERIFY_UNCERTAIN"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return s, fmt.Errorf("VERIFY_UNCERTAIN %w", errInvalidPositiveInteger)
		}
		s.uncertain = value
	}
	if raw := environ("VERIFY_EXPECTED_LOTS"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return s, fmt.Errorf("VERIFY_EXPECTED_LOTS %w", errInvalidPositiveInteger)
		}
		s.expectedLots = value
	}
	if raw := environ("VERIFY_WAIT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			return s, fmt.Errorf("VERIFY_WAIT %w", errInvalidPositiveDuration)
		}
		s.wait = value
	}
	switch s.mode {
	case modeBids:
		if s.lotTitle == "" {
			return s, errLotTitleRequired
		}
	case modeCatalog:
		if s.titlePrefix == "" {
			return s, errTitlePrefixRequired
		}
	}

	return s, nil
}

var (
	errInvalidPositiveInteger  = errors.New("must be a non-negative integer")
	errInvalidPositiveDuration = errors.New("must be a positive duration (for example 180s)")
)

type bidResult struct {
	Mode           string  `json:"mode"`
	LotID          int64   `json:"lot_id,omitempty"`
	LotTitle       string  `json:"lot_title"`
	Status         string  `json:"status"`
	EndsAt         string  `json:"ends_at,omitempty"`
	FinishedAt     string  `json:"finished_at,omitempty"`
	FinishDelay    float64 `json:"finish_delay_seconds,omitempty"`
	BidsCount      int64   `json:"bids_count"`
	MaxAmount      string  `json:"max_amount,omitempty"`
	MaxAmountBidID int64   `json:"max_amount_bid_id,omitempty"`
	WinningBidID   int64   `json:"winning_bid_id,omitempty"`
	WinningAmount  string  `json:"winning_amount,omitempty"`
	ExpectedNew    int64   `json:"expected_new"`
	Uncertain      int64   `json:"uncertain_outcomes"`
	Checks         []check `json:"checks"`
	Passed         bool    `json:"passed"`
}

type catalogResult struct {
	Mode         string  `json:"mode"`
	TitlePrefix  string  `json:"title_prefix"`
	LotsCount    int64   `json:"lots_count"`
	ActiveCount  int64   `json:"active_count"`
	BidsCount    int64   `json:"bids_count"`
	ExpectedLots int64   `json:"expected_lots"`
	Checks       []check `json:"checks"`
	Passed       bool    `json:"passed"`
}

// lotRow is the state of one lot as the verifier reads it from the database.
type lotRow struct {
	ID         int64
	Status     string
	EndsAt     time.Time
	FinishedAt pgtype.Timestamptz
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}
}

func run() error {
	settings, err := loadSettings(os.Getenv)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), settings.wait+30*time.Second)
	defer cancel()

	dbConfig := config.Database{URL: settings.databaseURL, MaxConns: 2, Timeout: 5 * time.Second}
	pool, err := postgres.Connect(ctx, dbConfig)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	var report any
	switch settings.mode {
	case modeBids:
		report, err = verifyBids(ctx, pool, settings)
	case modeCatalog:
		report, err = verifyCatalog(ctx, pool, settings)
	}
	if err != nil {
		return err
	}
	if err := emitReport(report, settings.output); err != nil {
		return err
	}
	if !reportPassed(report) {
		return errChecksFailed
	}

	return nil
}

func reportPassed(report any) bool {
	switch r := report.(type) {
	case bidResult:
		return r.Passed
	case catalogResult:
		return r.Passed
	}

	return false
}

func emitReport(report any, output string) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	fmt.Println(string(encoded))

	if output != "" {
		if err := os.WriteFile(output, encoded, 0o600); err != nil {
			return fmt.Errorf("write report file: %w", err)
		}
	}

	return nil
}

// verifyBids waits for the worker to finish the competed lot, then gathers
// the facts the checks decide on.
func verifyBids(ctx context.Context, pool *pgxpool.Pool, s settings) (bidResult, error) {
	result := bidResult{Mode: modeBids, LotTitle: s.lotTitle, ExpectedNew: s.expectedNew, Uncertain: s.uncertain}

	waitCtx, cancelWait := context.WithTimeout(ctx, s.wait)
	defer cancelWait()

	row, err := findLot(waitCtx, pool, s.lotTitle)
	if err != nil {
		return result, err
	}
	result.LotID = row.ID

	facts := bidFacts{ExpectedNew: s.expectedNew, UncertainOutcomes: s.uncertain}
	for {
		row, err = readLot(waitCtx, pool, row.ID)
		if err != nil {
			return result, err
		}
		if row.Status == "finished" {
			facts.Finished = true
			result.Status = row.Status
			result.EndsAt = row.EndsAt.UTC().Format(time.RFC3339)
			result.FinishedAt = row.FinishedAt.Time.UTC().Format(time.RFC3339)
			result.FinishDelay = row.FinishedAt.Time.Sub(row.EndsAt).Seconds()
			facts.FinishDelaySeconds = result.FinishDelay

			break
		}
		if waitCtx.Err() != nil {
			result.Status = row.Status

			break
		}
		time.Sleep(2 * time.Second)
	}

	if facts.Finished {
		bidFacts, err := readBidFacts(ctx, pool, row.ID, facts)
		if err != nil {
			return result, err
		}
		facts = bidFacts
		result.BidsCount = facts.BidsCount
		result.MaxAmount = strconv.FormatInt(facts.MaxAmount, 10)
		result.MaxAmountBidID = facts.MaxAmountBidID
		result.WinningBidID = facts.WinningBidID
		result.WinningAmount = strconv.FormatInt(facts.WinningAmount, 10)
	}
	result.Checks = bidChecks(facts)
	result.Passed = allPassed(result.Checks)

	return result, nil
}

// readBidFacts fills the missing bid fields of the running facts.
func readBidFacts(ctx context.Context, pool *pgxpool.Pool, lotID int64, facts bidFacts) (bidFacts, error) {
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COALESCE(MAX(amount), 0) FROM bids WHERE lot_id = $1", lotID,
	).Scan(&facts.BidsCount, &facts.MaxAmount)
	if err != nil {
		return facts, fmt.Errorf("count bids: %w", err)
	}

	err = pool.QueryRow(ctx,
		"SELECT id FROM bids WHERE lot_id = $1 ORDER BY amount DESC, id DESC LIMIT 1", lotID,
	).Scan(&facts.MaxAmountBidID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return facts, fmt.Errorf("find maximal bid: %w", err)
	}

	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM (SELECT amount - LAG(amount) OVER (ORDER BY id) AS d FROM bids WHERE lot_id = $1) s WHERE d <= 0", lotID,
	).Scan(&facts.DecreaseViolations)
	if err != nil {
		return facts, fmt.Errorf("check amount sequence: %w", err)
	}

	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM (SELECT DISTINCT user_id, request_key FROM bids WHERE lot_id = $1) d", lotID,
	).Scan(&facts.DistinctUserKeys)
	if err != nil {
		return facts, fmt.Errorf("count distinct request keys: %w", err)
	}

	var winningID *int64
	err = pool.QueryRow(ctx, "SELECT winning_bid_id FROM lots WHERE id = $1", lotID).Scan(&winningID)
	if err != nil {
		return facts, fmt.Errorf("read winning bid id: %w", err)
	}
	if winningID != nil {
		facts.HasWinningBid = true
		facts.WinningBidID = *winningID
		err = pool.QueryRow(ctx, "SELECT amount FROM bids WHERE id = $1", *winningID).Scan(&facts.WinningAmount)
		if err != nil {
			return facts, fmt.Errorf("read winning bid amount: %w", err)
		}
	}

	return facts, nil
}

// verifyCatalog reads the state of every lot carrying the title prefix.
func verifyCatalog(ctx context.Context, pool *pgxpool.Pool, s settings) (catalogResult, error) {
	result := catalogResult{Mode: modeCatalog, TitlePrefix: s.titlePrefix, ExpectedLots: s.expectedLots}

	pattern := likeEscape(s.titlePrefix) + "%"
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'active') FROM lots WHERE lower(title) LIKE lower($1)", pattern,
	).Scan(&result.LotsCount, &result.ActiveCount)
	if err != nil {
		return result, fmt.Errorf("count load lots: %w", err)
	}

	err = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM bids b JOIN lots l ON l.id = b.lot_id WHERE lower(l.title) LIKE lower($1)", pattern,
	).Scan(&result.BidsCount)
	if err != nil {
		return result, fmt.Errorf("count bids on load lots: %w", err)
	}

	result.Checks = catalogChecks(catalogFacts{
		LotsCount:    result.LotsCount,
		ActiveCount:  result.ActiveCount,
		BidsCount:    result.BidsCount,
		ExpectedLots: s.expectedLots,
	})
	result.Passed = allPassed(result.Checks)

	return result, nil
}

// likeEscape turns the prefix into a literal LIKE pattern fragment: % and _
// inside the prefix stop being wildcards.
func likeEscape(prefix string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

	return replacer.Replace(prefix)
}

func findLot(ctx context.Context, pool *pgxpool.Pool, title string) (lotRow, error) {
	var row lotRow
	err := pool.QueryRow(ctx,
		"SELECT id, status, ends_at, finished_at FROM lots WHERE lower(title) = lower($1)", title,
	).Scan(&row.ID, &row.Status, &row.EndsAt, &row.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lotRow{}, fmt.Errorf("%w: %s", errLotNotFound, title)
	}
	if err != nil {
		return lotRow{}, fmt.Errorf("find lot: %w", err)
	}

	return row, nil
}

func readLot(ctx context.Context, pool *pgxpool.Pool, id int64) (lotRow, error) {
	var row lotRow
	err := pool.QueryRow(ctx,
		"SELECT id, status, ends_at, finished_at FROM lots WHERE id = $1", id,
	).Scan(&row.ID, &row.Status, &row.EndsAt, &row.FinishedAt)
	if err != nil {
		return lotRow{}, fmt.Errorf("read lot %d: %w", id, err)
	}

	return row, nil
}
