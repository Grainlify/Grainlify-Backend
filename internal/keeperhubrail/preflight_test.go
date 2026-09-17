package keeperhubrail

import (
	"context"
	"errors"
	"testing"
)

// legAddresses reads every leg's frozen address for this fixture's hackathon,
// keyed by user id, so a test can target simulateUnsafe by the address a
// specific person will actually be paid at.
func (f *fx) legAddresses(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(), `
		SELECT l.user_id, l.address FROM keeperhub_payout_legs l
		JOIN keeperhub_payout_runs r ON r.id = l.run_id WHERE r.hackathon_id = $1`, f.hid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, addr string
		if err := rows.Scan(&uid, &addr); err != nil {
			t.Fatal(err)
		}
		out[uid] = addr
	}
	return out
}

// A release must simulate every leg, in order, before dispatching any of
// them.
func TestRelease_SimulatesEveryLegBeforeDispatch(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(rail.simCalls) != 3 {
		t.Fatalf("simulated %d legs, want 3 (one per sendable leg)", len(rail.simCalls))
	}
	if len(rail.calls) != 1 {
		t.Fatalf("dispatch called %d times, want exactly 1, and only after every simulation", len(rail.calls))
	}
	if len(res.Preflight) != 3 {
		t.Fatalf("release result carries %d preflight results, want 3", len(res.Preflight))
	}
	for i, p := range res.Preflight {
		if !p.Safe || p.WouldRevert || p.Unavailable {
			t.Errorf("preflight[%d] = %+v, want a clean safe result", i, p)
		}
		if p.LegID != res.LegIDs[i] {
			t.Errorf("preflight[%d].LegID = %s, want %s (same order as LegIDs)", i, p.LegID, res.LegIDs[i])
		}
	}
	// Every simulate call spent human-readable amounts, not minor units: 1
	// USDC is "1", not "1000000".
	for _, c := range rail.simCalls {
		if c.amount != "1" && c.amount != "2" && c.amount != "3" {
			t.Errorf("simulate amount = %q, want a human-readable USDC amount (1, 2 or 3)", c.amount)
		}
		if c.tokenAddress == "" {
			t.Error("simulate called with no token address")
		}
	}
}

// A leg that would revert refuses the WHOLE release: nothing is claimed,
// nothing is dispatched, and the other legs - which simulated safely - are
// not sent either.
func TestRelease_RefusesWhenALegWouldRevert(t *testing.T) {
	f := fixture(t)
	// Force the plan first so we know the addresses to target, matching the
	// same plan a real release would freeze.
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	runID, _, _, sendable, err := s.previewRun(context.Background(), f.req())
	if err != nil {
		t.Fatalf("previewRun: %v", err)
	}
	if len(sendable) != 3 {
		t.Fatalf("previewed %d sendable legs, want 3", len(sendable))
	}
	target := sendable[1].Address
	rail.simulateUnsafe = map[string]bool{target: true}

	_, err = s.Release(context.Background(), f.req())
	if !errors.Is(err, ErrPreflightWouldRevert) {
		t.Fatalf("err = %v, want ErrPreflightWouldRevert", err)
	}
	if len(rail.calls) != 0 {
		t.Error("a release refused by preflight reached Dispatch")
	}
	for u, st := range f.legStatuses(t) {
		if st != "pending" {
			t.Errorf("leg for %s is %q after a preflight refusal, want pending - nothing was claimed", u, st)
		}
	}
	var attempts int
	f.d.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM keeperhub_dispatch_attempts a
		JOIN keeperhub_payout_runs r ON r.id = a.run_id WHERE r.hackathon_id = $1`, f.hid).Scan(&attempts)
	if attempts != 0 {
		t.Errorf("a preflight refusal wrote %d dispatch attempt(s), want 0", attempts)
	}
	_ = runID
}

// Simulation being unreachable refuses the release exactly as hard as a leg
// that would revert, but as a distinguishable cause: an operator reading this
// fixes the rail's own tooling, not a leg.
func TestRelease_RefusesWhenSimulationIsUnavailable(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, simulateErr: errors.New("connection refused")}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	_, err := s.Release(context.Background(), f.req())
	if !errors.Is(err, ErrPreflightUnavailable) {
		t.Fatalf("err = %v, want ErrPreflightUnavailable", err)
	}
	if errors.Is(err, ErrPreflightWouldRevert) {
		t.Fatal("an unavailable simulator must not also read as ErrPreflightWouldRevert - they are different causes")
	}
	if len(rail.calls) != 0 {
		t.Error("a release refused because simulation was unavailable reached Dispatch")
	}
	for u, st := range f.legStatuses(t) {
		if st != "pending" {
			t.Errorf("leg for %s is %q after simulation was unavailable, want pending - nothing was claimed", u, st)
		}
	}
}

// A successful release persists the evidence that preflight ran, and RunView
// surfaces it per leg per attempt.
func TestRunView_ShowsWhatPreflightChecked(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}

	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatalf("RunView: %v", err)
	}
	if len(v.Attempts) != 1 || v.Attempts[0].ID != res.AttemptID {
		t.Fatalf("attempts = %+v, want exactly the one release's attempt", v.Attempts)
	}
	att := v.Attempts[0]
	if len(att.Legs) != 3 {
		t.Fatalf("attempt carries %d legs, want 3", len(att.Legs))
	}
	for _, l := range att.Legs {
		if l.PreflightCheckedAt == nil {
			t.Errorf("leg %s has no preflight_checked_at - no evidence the mandatory check ran", l.LegID)
		}
		if l.PreflightWouldRevert {
			t.Errorf("leg %s shows preflight_would_revert=true, but only safe legs are ever dispatched", l.LegID)
		}
	}
}

// A leg that drops out of the sendable set between the preflight preview and
// the claim - here, simulated by resolving one to confirmed by hand right
// after previewRun runs - refuses the whole claim rather than dispatching a
// stale or unsimulated set.
func TestClaimPreviewedLegs_RefusesOnDriftSincePreview(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	runID, _, _, sendable, err := s.previewRun(context.Background(), f.req())
	if err != nil {
		t.Fatalf("previewRun: %v", err)
	}
	preflight, err := s.simulateLegs(context.Background(), f.chain, sendable)
	if err != nil {
		t.Fatalf("simulateLegs: %v", err)
	}

	// Drift, without tripping the blocking-status refusal first: one
	// previewed leg is resolved to 'confirmed' behind previewRun's back -
	// exactly as an operator concurrently finding evidence it already paid
	// would leave it. 'confirmed' is neither blocking nor sendable, so the
	// run-level refusal check passes; the leg set itself has still shrunk.
	if _, err := f.d.Pool.Exec(context.Background(), `
		UPDATE keeperhub_payout_legs SET status = 'confirmed', tx_hash = '0xresolved', updated_at = now()
		WHERE id = $1`, sendable[0].ID); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = s.claimPreviewedLegs(context.Background(), f.req(), runID, sendable, preflight)
	if !errors.Is(err, ErrConcurrentRelease) {
		t.Fatalf("err = %v, want ErrConcurrentRelease", err)
	}
	if len(rail.calls) != 0 {
		t.Error("a claim refused for drift still reached Dispatch")
	}
}
