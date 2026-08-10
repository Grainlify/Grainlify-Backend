package hackathon

import (
	"context"
	"math/rand"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestWeightsFor_Section39 pins the §3.9 ticket arithmetic. Pure function,
// no DB - these are the numbers that decide who wins, so they get asserted
// directly rather than inferred from draw outcomes.
func TestWeightsFor_Section39(t *testing.T) {
	cfg := map[string]string{
		"weight_fit_strong":             "2.0",
		"weight_fit_plausible":          "1.0",
		"weight_fit_weak":               "0.25",
		"weight_difficulty_above":       "0.5",
		"weight_prior_completion":       "1.5",
		"weight_first_ever_application": "1.5",
		"weight_per_abandon":            "0.5",
	}

	tests := []struct {
		name string
		a    drawApplicant
		want float64
	}{
		{
			name: "first-ever plausible applicant: base 1.0 x plausible 1.0 x first-ever 1.5",
			a:    drawApplicant{fit: "plausible", diffMatch: "matched"},
			want: 1.5,
		},
		{
			// The newcomer-fairness case: a first-time applicant with a
			// strong fit should out-ticket an experienced weak-fit one.
			name: "strong fit, first ever",
			a:    drawApplicant{fit: "strong", diffMatch: "matched"},
			want: 3.0,
		},
		{
			name: "weak fit, has applied before",
			a:    drawApplicant{fit: "weak", diffMatch: "matched", priorApps: 3},
			want: 0.25,
		},
		{
			name: "difficulty above demonstrated level halves the tickets",
			a:    drawApplicant{fit: "plausible", diffMatch: "above", priorApps: 1},
			want: 0.5,
		},
		{
			name: "prior completions compound",
			a:    drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, completions: 2},
			want: 2.25, // 1.5^2
		},
		{
			name: "abandons compound as a penalty",
			a:    drawApplicant{fit: "plausible", diffMatch: "matched", priorApps: 5, abandons: 2},
			want: 0.25, // 0.5^2
		},
		{
			name: "difficulty 'below' is deliberately not penalised",
			a:    drawApplicant{fit: "plausible", diffMatch: "below", priorApps: 1},
			want: 1.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ticketsFrom(weightsFor(tc.a, cfg))
			if got != tc.want {
				t.Errorf("tickets = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPickWeighted_RespectsWeights is a statistical check that the draw
// actually honours ticket counts rather than picking uniformly.
func TestPickWeighted_RespectsWeights(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tickets := []float64{1, 3} // second candidate should win ~75%
	counts := [2]int{}
	for i := 0; i < 10000; i++ {
		counts[pickWeighted(rng, tickets)]++
	}
	ratio := float64(counts[1]) / 10000.0
	if ratio < 0.72 || ratio > 0.78 {
		t.Errorf("3:1 weighting produced %.3f share for the heavier candidate, want ~0.75", ratio)
	}
}

func TestPickWeighted_AllZeroTicketsReturnsNoWinner(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	if got := pickWeighted(rng, []float64{0, 0}); got != -1 {
		t.Errorf("pickWeighted with all-zero tickets = %d, want -1", got)
	}
}

// TestRunDraw_SameSeedReplaysExactly is the property an appeal depends on
// (§4.5.4: "store the seed on the draw record so any draw can be replayed").
func TestRunDraw_SameSeedReplaysExactly(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 1, "standard")

	for i := 0; i < 6; i++ {
		fxApplicant(t, pool, hackathonID, issueID, "applicant-"+string(rune('a'+i)), "plausible")
	}

	first, err := RunDraw(ctx, pool, hackathonID, issueID, 12345, true)
	if err != nil {
		t.Fatalf("first draw: %v", err)
	}
	second, err := RunDraw(ctx, pool, hackathonID, issueID, 12345, true)
	if err != nil {
		t.Fatalf("replay draw: %v", err)
	}

	if first.WinnerUserID == nil || second.WinnerUserID == nil {
		t.Fatalf("expected a winner in both draws, got %v and %v", first.WinnerUserID, second.WinnerUserID)
	}
	if *first.WinnerUserID != *second.WinnerUserID {
		t.Errorf("same seed produced different winners: %s vs %s", *first.WinnerUserID, *second.WinnerUserID)
	}
	if len(first.Pool) != len(second.Pool) {
		t.Fatalf("pool sizes differ: %d vs %d", len(first.Pool), len(second.Pool))
	}
	for i := range first.Pool {
		if first.Pool[i].Tickets != second.Pool[i].Tickets {
			t.Errorf("pool[%d] tickets differ: %v vs %v", i, first.Pool[i].Tickets, second.Pool[i].Tickets)
		}
	}
}

// TestRunDraw_SimulateWritesNoAssignment is the core guarantee of the
// admin "simulate draw" action.
func TestRunDraw_SimulateWritesNoAssignment(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 2, "standard")
	fxApplicant(t, pool, hackathonID, issueID, "sim-applicant", "plausible")

	res, err := RunDraw(ctx, pool, hackathonID, issueID, 999, true)
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if res.WinnerUserID == nil {
		t.Fatal("simulation should still pick a would-be winner")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_assignments WHERE hackathon_issue_id = $1`, issueID); n != 0 {
		t.Errorf("simulation wrote %d assignment(s), want 0", n)
	}
	// The applicant must stay in 'applied' - a simulation must not resolve
	// the real pool, or the real draw later would find nobody.
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_issue_applications WHERE hackathon_issue_id = $1 AND status = 'applied'`, issueID); n != 1 {
		t.Errorf("after simulation, applications still 'applied' = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_draws WHERE hackathon_issue_id = $1 AND is_simulation`, issueID); n != 1 {
		t.Errorf("simulation draw records = %d, want 1", n)
	}
}

// TestRunDraw_RealDrawAssignsAndResolvesPool covers the write path: winner
// assigned, slot consumed, everyone else marked lost.
func TestRunDraw_RealDrawAssignsAndResolvesPool(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 3, "standard")
	fxApplicant(t, pool, hackathonID, issueID, "real-a", "plausible")
	fxApplicant(t, pool, hackathonID, issueID, "real-b", "plausible")
	fxApplicant(t, pool, hackathonID, issueID, "real-c", "plausible")

	res, err := RunDraw(ctx, pool, hackathonID, issueID, 7, false)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if res.WinnerUserID == nil {
		t.Fatal("expected a winner")
	}

	if n := countRows(t, pool, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_issue_id = $1 AND user_id = $2 AND status = 'active' AND holds_slot`, issueID, *res.WinnerUserID); n != 1 {
		t.Errorf("winner's active slot-holding assignment = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_issue_applications WHERE hackathon_issue_id = $1 AND status = 'won'`, issueID); n != 1 {
		t.Errorf("won applications = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_issue_applications WHERE hackathon_issue_id = $1 AND status = 'lost'`, issueID); n != 2 {
		t.Errorf("lost applications = %d, want 2", n)
	}
}

// TestRunDraw_SequentialSlotConsumption is §4.5.5: a contributor who wins
// issue A must not also win issue B in the same batch once that would
// exceed their slots.
func TestRunDraw_SequentialSlotConsumption(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "slots_per_contributor", "1")

	issueA := fxPublishedIssue(t, pool, hackathonID, projectID, 10, "standard")
	issueB := fxPublishedIssue(t, pool, hackathonID, projectID, 11, "standard")

	// One contributor, sole applicant to both issues.
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "solo-applicant")
	fxApplication(t, pool, hackathonID, issueA, userID, "solo-applicant", "plausible")
	fxApplication(t, pool, hackathonID, issueB, userID, "solo-applicant", "plausible")

	if _, err := RunDraw(ctx, pool, hackathonID, issueA, 1, false); err != nil {
		t.Fatalf("draw A: %v", err)
	}
	resB, err := RunDraw(ctx, pool, hackathonID, issueB, 2, false)
	if err != nil {
		t.Fatalf("draw B: %v", err)
	}

	if resB.WinnerUserID != nil {
		t.Errorf("second draw assigned the same contributor a 2nd issue despite slots_per_contributor=1")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_assignments WHERE hackathon_id = $1 AND user_id = $2`, hackathonID, userID); n != 1 {
		t.Errorf("assignments for the slot-capped contributor = %d, want 1", n)
	}
	if resB.NoWinnerReason == "" {
		t.Error("expected the second draw to record why it produced no winner")
	}
}

// TestRunDraw_WeakPoolOnlyUsedWhenConfigured covers §4.5.1's fallback.
func TestRunDraw_WeakPoolOnlyUsedWhenConfigured(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	t.Run("falls back to weak when allowed", func(t *testing.T) {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 20, "standard")
		fxApplicant(t, pool, hackathonID, issueID, "weak-a", "weak")
		res, err := RunDraw(ctx, pool, hackathonID, issueID, 5, true)
		if err != nil {
			t.Fatalf("draw: %v", err)
		}
		if !res.UsedWeakPool {
			t.Error("expected used_weak_pool = true")
		}
		if res.WinnerUserID == nil {
			t.Error("expected the weak applicant to win rather than leaving the issue unassigned")
		}
	})

	t.Run("leaves the issue unassigned when disallowed", func(t *testing.T) {
		h2, p2, _ := fxLiveHackathon(t, pool)
		fxSetConfig(t, pool, h2, "draw_from_weak_pool_if_empty", "false")
		fxSetConfig(t, pool, h2, "fallback_to_first_come", "false")
		issueID := fxPublishedIssue(t, pool, h2, p2, 21, "standard")
		fxApplicant(t, pool, h2, issueID, "weak-b", "weak")

		res, err := RunDraw(ctx, pool, h2, issueID, 5, true)
		if err != nil {
			t.Fatalf("draw: %v", err)
		}
		if res.WinnerUserID != nil {
			t.Error("expected no winner when the weak pool is disallowed")
		}
	})
}

// TestRunDraw_NewcomerReservation covers §3.8: a reserved issue draws only
// from contributors with zero completed GrainHack issues.
func TestRunDraw_NewcomerReservation(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 30, "easy")
	if _, err := pool.Exec(ctx, `UPDATE hackathon_issues SET reserved = true WHERE id = $1`, issueID); err != nil {
		t.Fatalf("mark reserved: %v", err)
	}

	newcomer := fxApplicant(t, pool, hackathonID, issueID, "newcomer", "plausible")

	// A veteran with a completed assignment elsewhere, who must be excluded.
	veteran := fxUser(t, pool)
	fxGitHubAccount(t, pool, veteran, "veteran")
	fxApplication(t, pool, hackathonID, issueID, veteran, "veteran", "strong")
	otherIssue := fxPublishedIssue(t, pool, hackathonID, projectID, 31, "standard")
	vaID := fxAssignment(t, pool, hackathonID, otherIssue, projectID, veteran, 31, "veteran", nil)
	if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET status = 'completed', holds_slot = false WHERE id = $1`, vaID); err != nil {
		t.Fatalf("complete veteran assignment: %v", err)
	}

	res, err := RunDraw(ctx, pool, hackathonID, issueID, 3, true)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if !res.ReservationApplied {
		t.Error("expected reservation_applied = true")
	}
	if res.WinnerUserID == nil || *res.WinnerUserID != newcomer {
		t.Errorf("reserved issue winner = %v, want the newcomer %s (the veteran has a completion and must be excluded)", res.WinnerUserID, newcomer)
	}
	if len(res.Pool) != 1 {
		t.Errorf("reserved pool size = %d, want 1 (newcomer only)", len(res.Pool))
	}
}

// TestDecideReserved_ConvergesToTargetPercentage checks §3.8's running
// ratio directly, including the small-count behaviour where naive rounding
// would reserve nothing at all.
func TestDecideReserved_ConvergesToTargetPercentage(t *testing.T) {
	for _, pct := range []int{0, 30, 50, 100} {
		published, reserved := 0, 0
		for i := 0; i < 100; i++ {
			if decideReserved(published, reserved, pct) {
				reserved++
			}
			published++
		}
		got := reserved * 100 / published
		// Tolerance of one issue: the first issue in a tier is never
		// reserved (a one-issue tier must not be newcomer-only), so the
		// running count stays at most one behind the target forever.
		if diff := got - pct; diff > 1 || diff < -1 {
			t.Errorf("pct=%d produced %d%% reserved over 100 issues, want within 1 point", pct, got)
		}
	}

	// A one-issue tier reserves nothing: reserving it would make the only
	// issue at that difficulty newcomer-only.
	if decideReserved(0, 0, 50) != false {
		t.Error("a single issue at pct=50 must not be reserved - that would lock everyone else out of the tier")
	}
	if decideReserved(0, 0, 0) != false {
		t.Error("pct=0 must never reserve")
	}
	if decideReserved(0, 0, 100) != false {
		t.Error("even pct=100 must not reserve a tier's only issue")
	}

	// The max(1, ...) floor: small tiers still get a reserved issue, which
	// is the case a plain ratio rounds away - and it's the first event,
	// where newcomer reservation matters most.
	smallTiers := []struct {
		name                            string
		publishedInTier, reservedInTier int
		pct                             int
		want                            bool
	}{
		{"2 issues at 30% still reserves one", 1, 0, 30, true},
		{"3 issues at 30% still reserves one", 2, 0, 30, true},
		{"3 issues at 30%, one already reserved, stops at one", 2, 1, 30, false},
		{"2 issues at 50% reserves the second", 1, 0, 50, true},
		{"4 issues at 50% reserves two", 3, 1, 50, true},
		{"4 issues at 50%, two already reserved, stops", 3, 2, 50, false},
		{"10 issues at 0% never reserves", 9, 0, 0, false},
	}
	for _, tc := range smallTiers {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideReserved(tc.publishedInTier, tc.reservedInTier, tc.pct); got != tc.want {
				t.Errorf("decideReserved(%d, %d, %d) = %v, want %v",
					tc.publishedInTier, tc.reservedInTier, tc.pct, got, tc.want)
			}
		})
	}
}
