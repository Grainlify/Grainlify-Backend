package hackathon

import (
	"math"
	"testing"
	"time"
)

func drPR(id, login, bucket string, minutesAfter int) JudgedPR {
	return JudgedPR{
		VerdictID: id, Login: login, Bucket: bucket,
		MergedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(minutesAfter) * time.Minute),
	}
}

func entryFor(plan *PayoutPlan, verdictID string) PayoutEntry {
	for _, e := range plan.Entries {
		if e.VerdictID == verdictID {
			return e
		}
	}
	return PayoutEntry{}
}

// The curve reduces later PRs from the same contributor, and the units it
// removes raise the unit value for everyone else. That redistribution is the
// intent, so it is asserted directly rather than inferred.
func TestComputePayout_CurveRedistributesToOtherContributors(t *testing.T) {
	cfg := map[string]string{}

	// One prolific contributor with 3 accepted PRs, one with a single PR.
	prs := []JudgedPR{
		drPR("a1", "prolific", "accepted", 0),
		drPR("a2", "prolific", "accepted", 10),
		drPR("a3", "prolific", "accepted", 20),
		drPR("b1", "solo", "accepted", 5),
	}

	withCurve, err := ComputePayout(prs, 1000, cfg)
	if err != nil {
		t.Fatalf("ComputePayout: %v", err)
	}
	off := map[string]string{"diminishing_returns_enabled": "false"}
	without, err := ComputePayout(prs, 1000, off)
	if err != nil {
		t.Fatalf("ComputePayout (off): %v", err)
	}

	// 1.0 + 0.8 + 0.6 + 1.0 = 3.4 effective units against 4 flat.
	if math.Abs(withCurve.TotalUnits-3.4) > 0.001 {
		t.Errorf("total_units = %v, want 3.4", withCurve.TotalUnits)
	}
	if math.Abs(without.TotalUnits-4) > 0.001 {
		t.Errorf("total_units with the curve off = %v, want 4", without.TotalUnits)
	}

	// The single-PR contributor is paid strictly more because of the curve.
	soloWith := entryFor(withCurve, "b1").Amount
	soloWithout := entryFor(without, "b1").Amount
	if !(soloWith > soloWithout) {
		t.Errorf("the solo contributor got %v with the curve and %v without; the removed units should have raised their share", soloWith, soloWithout)
	}

	// And the prolific contributor's own PRs decline in order.
	first := entryFor(withCurve, "a1").Amount
	second := entryFor(withCurve, "a2").Amount
	third := entryFor(withCurve, "a3").Amount
	if !(first > second && second > third) {
		t.Errorf("a contributor's PRs did not decline: %v, %v, %v", first, second, third)
	}

	// The pool is still fully allocated.
	var total float64
	for _, e := range withCurve.Entries {
		total += e.Amount
	}
	if math.Abs(total-1000) > 0.05 {
		t.Errorf("payouts sum to %v, want the full 1000 pool", total)
	}
}

// Position is by merge time, and is recorded per PR so an appeal can see why
// a PR earned what it did.
func TestComputePayout_CurvePositionFollowsMergeOrderAndIsRecorded(t *testing.T) {
	// Deliberately supplied out of order.
	prs := []JudgedPR{
		drPR("late", "dev", "accepted", 100),
		drPR("early", "dev", "accepted", 1),
		drPR("middle", "dev", "accepted", 50),
	}
	plan, err := ComputePayout(prs, 900, map[string]string{})
	if err != nil {
		t.Fatalf("ComputePayout: %v", err)
	}

	want := map[string]struct {
		pos  int
		mult float64
	}{"early": {1, 1.0}, "middle": {2, 0.8}, "late": {3, 0.6}}
	for id, w := range want {
		e := entryFor(plan, id)
		if e.CurvePosition != w.pos {
			t.Errorf("%s: curve_position = %d, want %d", id, e.CurvePosition, w.pos)
		}
		if math.Abs(e.CurveMultiplier-w.mult) > 0.0001 {
			t.Errorf("%s: curve_multiplier = %v, want %v", id, e.CurveMultiplier, w.mult)
		}
		if math.Abs(e.EffectiveUnits-float64(e.Units)*w.mult) > 0.0001 {
			t.Errorf("%s: effective_units = %v, inconsistent with units %d x %v", id, e.EffectiveUnits, e.Units, w.mult)
		}
	}
	if !plan.CurveApplied || len(plan.Curve) == 0 {
		t.Error("the plan did not record the curve it used; a payout must be reproducible from its own snapshot")
	}
}

// The same person recorded with different capitalisation must get one curve,
// not two. Otherwise the loophole the curve exists to close is reopened by a
// difference in how GitHub happened to record a login.
func TestComputePayout_CurveGroupsLoginsCaseInsensitively(t *testing.T) {
	prs := []JudgedPR{
		drPR("x1", "Prolific", "accepted", 0),
		drPR("x2", "prolific", "accepted", 10),
		drPR("x3", "PROLIFIC", "accepted", 20),
	}
	plan, err := ComputePayout(prs, 300, map[string]string{})
	if err != nil {
		t.Fatalf("ComputePayout: %v", err)
	}
	positions := map[int]bool{}
	for _, e := range plan.Entries {
		positions[e.CurvePosition] = true
	}
	if !positions[1] || !positions[2] || !positions[3] {
		t.Errorf("expected positions 1, 2 and 3 across capitalisations, got %v", positions)
	}
}

// The last curve value repeats rather than running out.
func TestCurveMultiplier_LastValueRepeats(t *testing.T) {
	curve := ParseDiminishingCurve("[1.0, 0.8, 0.6, 0.5, 0.4]")
	for _, pos := range []int{5, 6, 12, 400} {
		if got := curveMultiplier(curve, pos); got != 0.4 {
			t.Errorf("position %d: multiplier = %v, want the repeating 0.4", pos, got)
		}
	}
	// A malformed curve falls back rather than zeroing every payout.
	if got := ParseDiminishingCurve("[1.0, banana]"); len(got) != len(DefaultDiminishingCurve) {
		t.Errorf("malformed curve did not fall back to the default: %v", got)
	}
}

// The floor is applied after the curve, and a PR that falls below it goes
// through payout_floor_strategy rather than being paid a token amount.
func TestComputePayout_FloorAppliesAfterTheCurve(t *testing.T) {
	cfg := map[string]string{"payout_floor": "100"}

	// 6 PRs from one contributor: later ones shrink to 0.4 units.
	var prs []JudgedPR
	for i := 0; i < 6; i++ {
		prs = append(prs, drPR(string(rune('a'+i))+"1", "prolific", "accepted", i*10))
	}
	plan, err := ComputePayout(prs, 300, cfg)
	if err != nil {
		t.Fatalf("ComputePayout: %v", err)
	}
	if !plan.FloorApplied {
		t.Fatalf("floor not applied; unit_value was %v against a floor of 100", plan.UnitValue)
	}

	// Nobody is paid a token amount: every funded entry is at least the
	// floor times its effective units, and unfunded ones are paid zero and
	// counted rather than quietly given a few pounds.
	for _, e := range plan.Entries {
		if e.Funded {
			want := round2(e.EffectiveUnits * plan.UnitValue)
			if math.Abs(e.Amount-want) > 0.01 {
				t.Errorf("%s funded at %v, want %v (effective units x floor)", e.VerdictID, e.Amount, want)
			}
		} else if e.Amount != 0 {
			t.Errorf("%s is unfunded but was paid %v", e.VerdictID, e.Amount)
		}
	}
	if plan.UnfundedCount == 0 {
		t.Error("expected the floor to leave later PRs explicitly unfunded")
	}

	// The pool is not overspent.
	var total float64
	for _, e := range plan.Entries {
		total += e.Amount
	}
	if total > 300.01 {
		t.Errorf("floor strategy overspent the pool: %v > 300", total)
	}
}

// With the curve off, behaviour matches the pre-curve arithmetic exactly.
func TestComputePayout_CurveOffMatchesFlatUnits(t *testing.T) {
	cfg := map[string]string{"diminishing_returns_enabled": "false"}
	prs := []JudgedPR{
		drPR("p1", "a", "accepted", 0),
		drPR("p2", "a", "substantial", 10),
		drPR("p3", "b", "exceptional", 20),
	}
	plan, err := ComputePayout(prs, 910, cfg)
	if err != nil {
		t.Fatalf("ComputePayout: %v", err)
	}
	// 1 + 3 + 5 = 9 units.
	if math.Abs(plan.TotalUnits-9) > 0.001 {
		t.Errorf("total_units = %v, want 9 with the curve off", plan.TotalUnits)
	}
	for _, e := range plan.Entries {
		if e.CurveMultiplier != 1 {
			t.Errorf("%s: multiplier = %v with the curve off, want 1", e.VerdictID, e.CurveMultiplier)
		}
	}
}
