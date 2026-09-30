package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findCheck returns the named check so the tests assert single decisions
// without depending on the order or the total number of checks.
func findCheck(t *testing.T, checks []check, name string) check {
	t.Helper()

	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	require.FailNow(t, "check not found", "no check named %q", name)

	return check{}
}

func TestWithinUncertaintyWindow(t *testing.T) {
	assert.True(t, withinUncertainty(10, 0, 10), "exact count with no uncertain outcomes fits")
	assert.True(t, withinUncertainty(10, 3, 12), "a stored uncertain outcome widens the window upward")
	assert.True(t, withinUncertainty(10, 3, 13), "the upper bound itself fits")
	assert.False(t, withinUncertainty(10, 3, 9), "fewer rows than confirmed bids is a loss")
	assert.False(t, withinUncertainty(10, 3, 14), "more rows than confirmed plus uncertain is a gain")
	assert.False(t, withinUncertainty(10, 3, 0), "zero rows never fits a non-zero expectation")
}

func TestBidChecksAllPass(t *testing.T) {
	facts := bidFacts{
		Finished:           true,
		FinishDelaySeconds: 1.25,
		BidsCount:          12,
		MaxAmount:          1050,
		MaxAmountBidID:     40,
		HasWinningBid:      true,
		WinningBidID:       40,
		WinningAmount:      1050,
		DecreaseViolations: 0,
		DistinctUserKeys:   12,
		ExpectedNew:        12,
		UncertainOutcomes:  0,
	}

	checks := bidChecks(facts)

	for _, c := range checks {
		assert.True(t, c.Passed, "check %s must pass: %s", c.Name, c.Detail)
	}
	assert.Equal(t, "1.25", findCheck(t, checks, "worker_finished").Detail)
	assert.Equal(t, int64(12), findCheck(t, checks, "bids_present").DetailInt)
}

func TestBidChecksReconcilesUncertainOutcomes(t *testing.T) {
	facts := bidFacts{
		Finished:          true,
		BidsCount:         13,
		MaxAmount:         1050,
		MaxAmountBidID:    40,
		HasWinningBid:     true,
		WinningBidID:      40,
		WinningAmount:     1050,
		DistinctUserKeys:  13,
		ExpectedNew:       12,
		UncertainOutcomes: 1,
	}

	checks := bidChecks(facts)

	assert.True(t, findCheck(t, checks, "count_matches_confirmed").Passed,
		"one stored uncertain outcome is a legitimate extra row")
	assert.True(t, findCheck(t, checks, "request_keys_unique").Passed)
}

func TestBidChecksFailures(t *testing.T) {
	testCases := []struct {
		name      string
		mutate    func(*bidFacts)
		checkName string
	}{
		{
			name:      "worker did not finish the lot",
			mutate:    func(f *bidFacts) { f.Finished = false },
			checkName: "worker_finished",
		},
		{
			name:      "no bids were stored",
			mutate:    func(f *bidFacts) { f.BidsCount = 0; f.DistinctUserKeys = 0 },
			checkName: "bids_present",
		},
		{
			name:      "fewer rows than confirmed bids",
			mutate:    func(f *bidFacts) { f.BidsCount = 11; f.DistinctUserKeys = 11 },
			checkName: "count_matches_confirmed",
		},
		{
			name:      "more rows than confirmed and uncertain",
			mutate:    func(f *bidFacts) { f.BidsCount = 14; f.DistinctUserKeys = 14 },
			checkName: "count_matches_confirmed",
		},
		{
			name:      "winner is not the maximal bid",
			mutate:    func(f *bidFacts) { f.WinningBidID = 39 },
			checkName: "winner_is_maximal_bid",
		},
		{
			name:      "winning amount differs from the maximum",
			mutate:    func(f *bidFacts) { f.WinningAmount = 1000 },
			checkName: "winner_amount_matches_max",
		},
		{
			name:      "accepted sequence decreased once",
			mutate:    func(f *bidFacts) { f.DecreaseViolations = 1 },
			checkName: "amounts_strictly_increase",
		},
		{
			name:      "a request key was stored twice",
			mutate:    func(f *bidFacts) { f.DistinctUserKeys = 11 },
			checkName: "request_keys_unique",
		},
		{
			name:      "no winner despite bids",
			mutate:    func(f *bidFacts) { f.HasWinningBid = false; f.WinningBidID = 0; f.WinningAmount = 0 },
			checkName: "winner_is_maximal_bid",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			facts := bidFacts{
				Finished:          true,
				BidsCount:         12,
				MaxAmount:         1050,
				MaxAmountBidID:    40,
				HasWinningBid:     true,
				WinningBidID:      40,
				WinningAmount:     1050,
				DistinctUserKeys:  12,
				ExpectedNew:       12,
				UncertainOutcomes: 0,
			}
			tc.mutate(&facts)

			checks := bidChecks(facts)

			target := findCheck(t, checks, tc.checkName)
			assert.False(t, target.Passed, "the mutated fact must fail the check")
			assert.NotEmpty(t, target.Detail, "a failing check must explain itself")
		})
	}
}

func TestCatalogChecksAllPass(t *testing.T) {
	facts := catalogFacts{
		LotsCount:    60,
		ActiveCount:  60,
		BidsCount:    0,
		ExpectedLots: 60,
	}

	checks := catalogChecks(facts)

	for _, c := range checks {
		assert.True(t, c.Passed, "check %s must pass: %s", c.Name, c.Detail)
	}
}

func TestCatalogChecksFailures(t *testing.T) {
	testCases := []struct {
		name      string
		mutate    func(*catalogFacts)
		checkName string
	}{
		{
			name:      "fewer lots than prepared",
			mutate:    func(f *catalogFacts) { f.LotsCount = 59 },
			checkName: "lots_prepared",
		},
		{
			name:      "a load lot left its active state",
			mutate:    func(f *catalogFacts) { f.ActiveCount = 59 },
			checkName: "lots_stay_active",
		},
		{
			name:      "a bid appeared on a read-only lot",
			mutate:    func(f *catalogFacts) { f.BidsCount = 1 },
			checkName: "reads_did_not_bid",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			facts := catalogFacts{
				LotsCount:    60,
				ActiveCount:  60,
				BidsCount:    0,
				ExpectedLots: 60,
			}
			tc.mutate(&facts)

			checks := catalogChecks(facts)

			target := findCheck(t, checks, tc.checkName)
			assert.False(t, target.Passed, "the mutated fact must fail the check")
			assert.NotEmpty(t, target.Detail, "a failing check must explain itself")
		})
	}
}

func TestCatalogChecksSkipsExpectationWhenUnset(t *testing.T) {
	facts := catalogFacts{
		LotsCount:    3,
		ActiveCount:  3,
		BidsCount:    0,
		ExpectedLots: 0,
	}

	checks := catalogChecks(facts)

	lotsPrepared := findCheck(t, checks, "lots_prepared")
	assert.True(t, lotsPrepared.Passed, "an unset expectation skips the count comparison")
	assert.Contains(t, lotsPrepared.Detail, "skipped")
}
