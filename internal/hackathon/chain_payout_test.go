package hackathon

import (
	"math"
	"testing"
)

// The diminishing-returns curve is scoped to a chain, not to the event.
//
// **Do not "fix" this to be global.** A global curve would discount one
// sponsor's pool because of activity in a pool they have no relationship to,
// which contradicts the separate-pool model the on-chain design rests on
// (§2: each chain is financially its own event). The consequence - working
// across two chains restarts the curve on each - is published on the rules
// page as a known property, and the global slot limits still cap how much
// work one person can hold at once regardless of how it is spread.
func TestComputePayoutsPerChain_CurveIsScopedToAChain(t *testing.T) {
	cfg := map[string]string{}

	// One contributor with two accepted PRs on each of two chains.
	prsByChain := map[string][]JudgedPR{
		"soroban": {
			drPR("s1", "dev", "accepted", 0),
			drPR("s2", "dev", "accepted", 10),
		},
		"flare": {
			drPR("f1", "dev", "accepted", 5),
			drPR("f2", "dev", "accepted", 15),
		},
	}
	pools := map[string]float64{"soroban": 1000, "flare": 1000}

	out, err := ComputePayoutsPerChain(prsByChain, pools, cfg)
	if err != nil {
		t.Fatalf("ComputePayoutsPerChain: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d chain plans, want 2", len(out))
	}

	for _, cp := range out {
		positions := map[int]bool{}
		for _, e := range cp.Plan.Entries {
			positions[e.CurvePosition] = true
		}
		// Each chain restarts at position 1. Globally scoped, this
		// contributor's third and fourth PRs would sit at 0.6 and 0.5.
		if !positions[1] || !positions[2] {
			t.Errorf("%s: curve positions = %v, want a fresh 1 and 2 on each chain", cp.ChainID, positions)
		}
		if positions[3] || positions[4] {
			t.Errorf("%s: curve continued across chains (positions %v); it must restart per chain", cp.ChainID, positions)
		}
		// 1.0 + 0.8 = 1.8 effective units on each chain independently.
		if math.Abs(cp.Plan.TotalUnits-1.8) > 0.001 {
			t.Errorf("%s: total_units = %v, want 1.8", cp.ChainID, cp.Plan.TotalUnits)
		}
	}
}

// A chain's pool pays only that chain's contributors. Money never moves
// between chains, at any point, for any reason (§2).
func TestComputePayoutsPerChain_PoolsNeverCross(t *testing.T) {
	prsByChain := map[string][]JudgedPR{
		"soroban": {drPR("s1", "alice", "accepted", 0)},
		"flare":   {drPR("f1", "bob", "accepted", 0), drPR("f2", "carol", "accepted", 1)},
	}
	// Deliberately lopsided: §2.3's stated consequence is that identical work
	// pays differently on different chains, and that is the model working.
	pools := map[string]float64{"soroban": 900, "flare": 100}

	out, err := ComputePayoutsPerChain(prsByChain, pools, map[string]string{})
	if err != nil {
		t.Fatalf("ComputePayoutsPerChain: %v", err)
	}

	byChain := map[string]ChainPayout{}
	for _, cp := range out {
		byChain[cp.ChainID] = cp
	}

	var sorobanTotal, flareTotal float64
	for _, e := range byChain["soroban"].Plan.Entries {
		sorobanTotal += e.Amount
		if e.Login != "alice" {
			t.Errorf("soroban paid %q, who did not work on that chain", e.Login)
		}
	}
	for _, e := range byChain["flare"].Plan.Entries {
		flareTotal += e.Amount
		if e.Login == "alice" {
			t.Error("alice was paid from flare's pool; money must never cross chains")
		}
	}
	if math.Abs(sorobanTotal-900) > 0.05 {
		t.Errorf("soroban paid out %v, want its own 900 pool", sorobanTotal)
	}
	if math.Abs(flareTotal-100) > 0.05 {
		t.Errorf("flare paid out %v, want its own 100 pool", flareTotal)
	}
	// The same single accepted PR is worth far more on the emptier-competition
	// chain. Asserted so the property is documented rather than surprising.
	if !(byChain["soroban"].Plan.UnitValue > byChain["flare"].Plan.UnitValue) {
		t.Error("expected per-chain unit values to differ with pool size and competition")
	}
}

// An event with no chain pools behaves exactly as before: one pool, one plan.
func TestComputePayoutsPerChain_NoChainsIsTheExistingPath(t *testing.T) {
	out, err := ComputePayoutsPerChain(
		map[string][]JudgedPR{"": {drPR("v1", "dev", "accepted", 0)}},
		map[string]float64{"": 500},
		map[string]string{},
	)
	if err != nil {
		t.Fatalf("ComputePayoutsPerChain: %v", err)
	}
	if len(out) != 1 || out[0].ChainID != "" {
		t.Fatalf("got %+v, want a single unchained plan", out)
	}
	if math.Abs(out[0].Plan.Entries[0].Amount-500) > 0.05 {
		t.Errorf("amount = %v, want the whole 500 pool", out[0].Plan.Entries[0].Amount)
	}
}
