package hackathon

import (
	"math/rand"
	"sort"
	"testing"
)

// This file is a weight-tuning instrument as much as a test. AI-specs.md
// §4.5 says to publish the draw weights before an event, which means they
// have to be chosen deliberately rather than discovered once real
// contributors have already planned around them. Run it with -v to read the
// table.
//
// It works on weightsFor/pickWeighted directly rather than through RunDraw:
// the DB path is covered elsewhere, and going straight at the arithmetic
// lets this sweep thousands of draws in milliseconds.

type cohort struct {
	name string
	a    drawApplicant
}

// realisticPool is a deliberately ordinary mid-event pool: mostly newcomers
// and returning applicants, a few veterans who have already completed
// issues, one contributor carrying an abandon.
func realisticPool() []cohort {
	return []cohort{
		{"newcomer (plausible)", drawApplicant{fit: "plausible", diffMatch: "matched"}},
		{"newcomer (plausible)", drawApplicant{fit: "plausible", diffMatch: "matched"}},
		{"newcomer (plausible)", drawApplicant{fit: "plausible", diffMatch: "matched"}},
		{"newcomer (strong)", drawApplicant{fit: "strong", diffMatch: "matched"}},
		{"newcomer (weak)", drawApplicant{fit: "weak", diffMatch: "matched"}},

		{"returning, no wins (plausible)", drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 3}},
		{"returning, no wins (plausible)", drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 6}},
		{"returning, no wins (strong)", drawApplicant{fit: "strong", diffMatch: "matched", priorApps: 2}},

		{"veteran, 1 completion", drawApplicant{fit: "strong", diffMatch: "matched", priorApps: 4, completions: 1}},
		{"veteran, 2 completions", drawApplicant{fit: "strong", diffMatch: "matched", priorApps: 8, completions: 2}},
		{"veteran, 3 completions", drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 11, completions: 3}},

		{"has one abandon", drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, abandons: 1}},
	}
}

func specDefaultWeights() map[string]string {
	return map[string]string{
		"weight_fit_strong":             "2.0",
		"weight_fit_plausible":          "1.0",
		"weight_fit_weak":               "0.25",
		"weight_difficulty_above":       "0.5",
		"weight_prior_completion":       "1.5",
		"weight_first_ever_application": "1.5",
		"weight_per_abandon":            "0.5",
	}
}

// isNewcomer matches the §3.8 definition the draw itself uses:
// zero completed GrainHack issues.
func isNewcomer(a drawApplicant) bool { return a.completions == 0 }

func TestWeightDistribution_RealisticPool(t *testing.T) {
	pool := realisticPool()
	cfg := specDefaultWeights()

	tickets := make([]float64, len(pool))
	total := 0.0
	for i, c := range pool {
		tickets[i] = ticketsFrom(weightsFor(c.a, cfg))
		total += tickets[i]
	}

	const draws = 200000
	rng := rand.New(rand.NewSource(20260810))
	wins := make([]int, len(pool))
	for i := 0; i < draws; i++ {
		wins[pickWeighted(rng, tickets)]++
	}

	t.Logf("Pool of %d, %d simulated draws, spec-default weights", len(pool), draws)
	t.Logf("%-32s %8s %8s %8s", "cohort", "tickets", "share", "wins%")
	for i, c := range pool {
		t.Logf("%-32s %8.3f %7.1f%% %7.1f%%",
			c.name, tickets[i], 100*tickets[i]/total, 100*float64(wins[i])/draws)
	}

	// Group by newcomer status - the §3.8 question.
	var newcomerTickets, veteranTickets float64
	var newcomerCount, veteranCount int
	for i, c := range pool {
		if isNewcomer(c.a) {
			newcomerTickets += tickets[i]
			newcomerCount++
		} else {
			veteranTickets += tickets[i]
			veteranCount++
		}
	}
	newcomerSeatShare := 100 * float64(newcomerCount) / float64(len(pool))
	newcomerWinShare := 100 * newcomerTickets / total
	t.Logf("")
	t.Logf("newcomers: %d of %d applicants (%.0f%% of the pool) -> %.1f%% of the odds",
		newcomerCount, len(pool), newcomerSeatShare, newcomerWinShare)
	t.Logf("with prior completions: %d of %d (%.0f%%) -> %.1f%% of the odds",
		veteranCount, len(pool), 100*float64(veteranCount)/float64(len(pool)), 100*veteranTickets/total)

	// The guard: newcomers must not be a rounding error. They are the
	// majority of any healthy pool, and if the weights concentrate odds on
	// people who have already won, the event stops recruiting anyone new -
	// which is the whole failure mode §3.9's "not available as weights"
	// list and §4.4's fairness rules exist to prevent.
	if newcomerWinShare < 20 {
		t.Errorf("newcomers hold %.1f%% of the odds while being %.0f%% of the pool - the weights have concentrated the event on prior winners",
			newcomerWinShare, newcomerSeatShare)
	}
}

// TestTicketOrdering_AccumulatedWinsNeverOutrankCapability is the permanent
// guard on the ordering the clamp exists to protect: **having won before
// must never, on its own, outrank demonstrating capability for this issue.**
//
// Stated precisely, because the loose version ("nobody exceeds a strong-fit
// newcomer") is wrong and would fail on a legitimate case: a veteran who
// *also* has a strong fit deserves both multipliers, and 4.5 > 3.0 there is
// correct. What must hold is that accumulated wins alone can't get you past
// someone who demonstrated they can do the work:
//
//   - a plausible-fit contributor, with any number of completions, stays
//     below a strong-fit first-timer, and
//   - the prior_completion multiplier itself stays bounded.
//
// Uncapped, both fail: the first crosses over at 3 completions and the
// second grows without limit. If this test starts failing, the clamp has
// been weakened - re-read weightsFor before changing the expectation.
func TestTicketOrdering_AccumulatedWinsNeverOutrankCapability(t *testing.T) {
	cfg := specDefaultWeights()
	strongNewcomer := ticketsFrom(weightsFor(drawApplicant{fit: "strong", diffMatch: "matched"}, cfg))

	// Far past any plausible event length, to prove the bound is structural
	// and not just "large enough for now".
	for _, completions := range []int{1, 2, 3, 5, 10, 50, 1000} {
		veteran := drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 20, completions: completions}
		w := weightsFor(veteran, cfg)

		if got := ticketsFrom(w); got >= strongNewcomer {
			t.Errorf("a plausible-fit contributor with %d completions holds %.3f tickets, at or above a strong-fit first-timer's %.3f - accumulated wins have overtaken demonstrated capability",
				completions, got, strongNewcomer)
		}
		if f := w["prior_completion"]; f > 2.25 {
			t.Errorf("prior_completion multiplier at %d completions = %.3f, want it clamped at 1.5^%d = 2.25",
				completions, f, priorCompletionCap)
		}
	}

	// The clamp must not erase the incentive entirely: one and two
	// completions still count for something.
	one := ticketsFrom(weightsFor(drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, completions: 1}, cfg))
	two := ticketsFrom(weightsFor(drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, completions: 2}, cfg))
	none := ticketsFrom(weightsFor(drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5}, cfg))
	if !(none < one && one < two) {
		t.Errorf("completions should still be rewarded up to the clamp: 0=%.3f 1=%.3f 2=%.3f", none, one, two)
	}
}

// TestWeightDistribution_PriorCompletionCompounding shows the clamp doing
// its job. weight_prior_completion is applied *per* completion (§3.9), so
// uncapped it was the only exponential term in an otherwise linear formula
// and dominated any mature pool; priorCompletionCap flattens it at 2.
func TestWeightDistribution_PriorCompletionCompounding(t *testing.T) {
	cfg := specDefaultWeights()
	newcomerStrong := ticketsFrom(weightsFor(drawApplicant{fit: "strong", diffMatch: "matched"}, cfg))

	t.Logf("a first-time applicant with a STRONG fit holds %.3f tickets", newcomerStrong)
	t.Logf("%-24s %10s %14s", "veteran completions", "tickets", "vs newcomer")
	for _, completions := range []int{0, 1, 2, 3, 4, 5} {
		v := ticketsFrom(weightsFor(drawApplicant{
			fit: "plausible", diffMatch: "matched", priorApps: 5, completions: completions,
		}, cfg))
		t.Logf("%-24d %10.3f %13.2fx", completions, v, v/newcomerStrong)
	}

	// With the clamp in place the table flattens at 2 completions and stays
	// under the strong-fit newcomer forever. Uncapped it crossed over at 3
	// and kept climbing.
	v3 := ticketsFrom(weightsFor(drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, completions: 3}, cfg))
	v50 := ticketsFrom(weightsFor(drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, completions: 50}, cfg))
	if v3 != v50 {
		t.Errorf("tickets still grow past the clamp: 3 completions = %.3f, 50 = %.3f", v3, v50)
	}
}

// TestWeightDistribution_FirstEverSweep answers the specific tuning
// question: how much does weight_first_ever_application actually buy a
// newcomer, and where does it stop mattering?
func TestWeightDistribution_FirstEverSweep(t *testing.T) {
	pool := realisticPool()

	t.Logf("%-28s %18s", "weight_first_ever", "newcomer odds share")
	type row struct {
		w     string
		share float64
	}
	var rows []row
	for _, w := range []string{"1.0", "1.5", "2.0", "2.5", "3.0", "4.0"} {
		cfg := specDefaultWeights()
		cfg["weight_first_ever_application"] = w

		var newcomerTickets, total float64
		for _, c := range pool {
			tk := ticketsFrom(weightsFor(c.a, cfg))
			total += tk
			if isNewcomer(c.a) {
				newcomerTickets += tk
			}
		}
		share := 100 * newcomerTickets / total
		rows = append(rows, row{w, share})
		t.Logf("%-28s %17.1f%%", w, share)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].share < rows[j].share })
	if rows[0].share >= rows[len(rows)-1].share {
		t.Error("raising weight_first_ever_application did not raise the newcomer share - the sweep is measuring nothing")
	}
}
