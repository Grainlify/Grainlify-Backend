package reconciler_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainops"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/reconciler"
)

// --- fakes -----------------------------------------------------------------

type fakeChain struct {
	claimed map[string]bool
	root    []byte
	pub     bool
	err     error
	reads   int
}

func (f *fakeChain) IsClaimed(_ context.Context, _ string, leaf []byte) (bool, error) {
	f.reads++
	if f.err != nil {
		return false, f.err
	}
	return f.claimed[hex.EncodeToString(leaf)], nil
}
func (f *fakeChain) PublishedRoot(context.Context, string) ([]byte, bool, error) {
	return f.root, f.pub, nil
}

type fakeAlerter struct{ sent []string }

func (a *fakeAlerter) AlertPayout(_ context.Context, subject, _ string) error {
	a.sent = append(a.sent, subject)
	return nil
}

// --- fixtures --------------------------------------------------------------

// leafFor derives a leaf that is unique to one event.
//
// The first version used the same 32 bytes of 0x01 in every run. Those tests
// passed on a clean database and failed on every run after it, because the alert
// dedupe is keyed on (kind, subject) and subject is the leaf hex - so the second
// run found the alert already claimed and no alert was sent.
//
// A fixture assumption nobody chose, exactly trap 11, and caught by the mutation
// harness refusing to measure anything against a baseline that did not pass.
// Mixing the settlement id in is also what real leaves do: they are per event.
func leafFor(ev uuid.UUID, b byte) []byte {
	out := make([]byte, 32)
	copy(out, ev[:])
	for i := 16; i < 32; i++ {
		out[i] = b
	}
	return out
}

// leaf is only for values that are not leaves - roots, and deliberately
// unrecognised digests.
func leaf(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

const escrowAddr = "0xdd4fa63ec44e4726cd7d1118596866cd0c5d4cd4d3fcc9762a42261872795f45"

// event creates a settlement, a recorded root, and claim rows in given states.
func event(t *testing.T, d *db.DB, root []byte, leaves map[byte]chainops.State) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var settlement uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO settlements (pool_usdc, pool_minor, total_weight, unit_value_usdc)
VALUES (1000, 1000000000, 10, 100) RETURNING id`).Scan(&settlement); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO payout_event_roots (settlement_id, chain_id, escrow_address, root, total_minor, leaf_count)
VALUES ($1, 'aptos-testnet', $2, $3, 1000000, $4)`,
		settlement, escrowAddr, root, len(leaves)); err != nil {
		t.Fatalf("recorded root: %v", err)
	}
	for b, state := range leaves {
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref, leaf_hash)
VALUES ('aptos-testnet', 'claim', $1, $2, $3)`, string(state), settlement, leafFor(settlement, b)); err != nil {
			t.Fatalf("claim row: %v", err)
		}
	}
	return settlement
}

func stateOf(t *testing.T, d *db.DB, ev uuid.UUID, b byte) chainops.State {
	t.Helper()
	var s string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT state FROM chain_operations WHERE event_ref = $1 AND leaf_hash = $2`,
		ev, leafFor(ev, b)).Scan(&s); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return chainops.State(s)
}

// --- tests -----------------------------------------------------------------

// A claim seen on chain is promoted, and only that one.
func TestReconcile_PromotesOnlyWhatTheChainConfirms(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{
		0x01: chainops.StateConfirmed,
		0x02: chainops.StateConfirmed,
	})

	chain := &fakeChain{root: root, pub: true, claimed: map[string]bool{
		hex.EncodeToString(leafFor(ev, 0x01)): true,
	}}
	r := reconciler.New(d.Pool, chain, chain, &fakeAlerter{})

	n, err := r.ReconcileClaims(context.Background(), ev)
	if err != nil {
		t.Fatalf("ReconcileClaims: %v", err)
	}
	if n != 1 {
		t.Errorf("promoted %d, want 1", n)
	}
	if got := stateOf(t, d, ev, 0x01); got != chainops.StatePaid {
		t.Errorf("claimed leaf state = %s, want paid", got)
	}
	if got := stateOf(t, d, ev, 0x02); got != chainops.StateConfirmed {
		t.Errorf("unclaimed leaf state = %s, want confirmed - an unclaimed leaf is not a problem", got)
	}
}

// Paid-but-unclaimed alerts and changes nothing. The row is NOT reverted:
// reverting is a guess about which side is wrong, and re-paying is how a guess
// becomes a double payment.
func TestReconcile_PaidButUnclaimedAlertsAndDoesNotAct(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{0x03: chainops.StatePaid})

	chain := &fakeChain{root: root, pub: true, claimed: map[string]bool{}}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	if _, err := r.ReconcileClaims(context.Background(), ev); err != nil {
		t.Fatalf("ReconcileClaims: %v", err)
	}
	if len(alerter.sent) != 1 || alerter.sent[0] != "paid_but_unclaimed" {
		t.Fatalf("alerts = %v, want one paid_but_unclaimed", alerter.sent)
	}
	if got := stateOf(t, d, ev, 0x03); got != chainops.StatePaid {
		t.Errorf("state = %s, want paid unchanged - the reconciler must not pick a direction", got)
	}
}

// Once per disagreement, not once per pass. A repeating alert gets muted, and a
// muted alert is worse than none because it still looks like coverage.
func TestReconcile_AlertsOncePerDisagreementNotOncePerRun(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{0x04: chainops.StatePaid})

	chain := &fakeChain{root: root, pub: true, claimed: map[string]bool{}}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	for i := 0; i < 5; i++ {
		if _, err := r.ReconcileClaims(context.Background(), ev); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if len(alerter.sent) != 1 {
		t.Errorf("five passes produced %d alerts, want 1", len(alerter.sent))
	}
}

// A root mismatch stops the whole event: every proof being served may be
// invalid, so settling individual leaves against it would be settling against a
// tree we do not understand.
func TestReconcile_RootMismatchStopsTheEventAndSettlesNothing(t *testing.T) {
	d := dbtest.DB(t)
	ev := event(t, d, leaf(0xEE), map[byte]chainops.State{0x05: chainops.StateConfirmed})

	chain := &fakeChain{root: leaf(0xFF), pub: true, claimed: map[string]bool{
		hex.EncodeToString(leafFor(ev, 0x05)): true, // would otherwise be promoted
	}}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	n, err := r.ReconcileClaims(context.Background(), ev)
	if !errors.Is(err, reconciler.ErrRootMismatch) {
		t.Fatalf("error = %v, want ErrRootMismatch", err)
	}
	if n != 0 {
		t.Errorf("promoted %d against a root we do not recognise, want 0", n)
	}
	if got := stateOf(t, d, ev, 0x05); got != chainops.StateConfirmed {
		t.Errorf("state = %s, want confirmed - nothing may settle under a mismatched root", got)
	}
	if len(alerter.sent) != 1 || alerter.sent[0] != "root_mismatch" {
		t.Errorf("alerts = %v, want one root_mismatch", alerter.sent)
	}
}

// A read failure is not a disagreement. Alerting on one would fire on every
// network blip and train the recipient to ignore it.
func TestReconcile_AReadFailureIsNotADisagreement(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{0x06: chainops.StateConfirmed})

	chain := &fakeChain{root: root, pub: true, err: errors.New("rpc timeout")}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	if _, err := r.ReconcileClaims(context.Background(), ev); err != nil {
		t.Fatalf("a read failure must not fail the pass: %v", err)
	}
	if len(alerter.sent) != 0 {
		t.Errorf("alerts = %v, want none", alerter.sent)
	}
	if got := stateOf(t, d, ev, 0x06); got != chainops.StateConfirmed {
		t.Errorf("state = %s, want unchanged", got)
	}
}

// The case the first version of the read-failure test missed.
//
// A read error on a CONFIRMED row is harmless either way. On a PAID row it is
// not: without the early return, the switch falls through to
// "!claimed && state == paid" and raises "we recorded a payment the chain does
// not have" - which is a false alarm produced by a network blip, and precisely
// the alert-fatigue this design claims to avoid.
//
// Found by a mutation that survived the first test because that test used the
// wrong state.
func TestReconcile_AReadFailureOnAPaidRowDoesNotRaiseAFalseAlarm(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{0x0A: chainops.StatePaid})

	chain := &fakeChain{root: root, pub: true, err: errors.New("rpc timeout")}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	if _, err := r.ReconcileClaims(context.Background(), ev); err != nil {
		t.Fatalf("read failure must not fail the pass: %v", err)
	}
	if len(alerter.sent) != 0 {
		t.Errorf("alerts = %v, want none - a failed read is not evidence of anything", alerter.sent)
	}
	if got := stateOf(t, d, ev, 0x0A); got != chainops.StatePaid {
		t.Errorf("state = %s, want paid unchanged", got)
	}
}

// Polling alone is sufficient. There is no trigger in this test and the end
// state is the settled one - which is the property that makes a missed trigger a
// delay rather than a lost payment.
func TestReconcile_PollingAloneReachesTheSettledState(t *testing.T) {
	d := dbtest.DB(t)
	root := leaf(0xEE)
	ev := event(t, d, root, map[byte]chainops.State{
		0x07: chainops.StateConfirmed,
		0x08: chainops.StateConfirmed,
	})

	chain := &fakeChain{root: root, pub: true, claimed: map[string]bool{}}
	r := reconciler.New(d.Pool, chain, chain, &fakeAlerter{})

	// Nothing claimed yet: a pass settles nothing and that is correct.
	if n, _ := r.ReconcileClaims(context.Background(), ev); n != 0 {
		t.Fatalf("promoted %d before anybody claimed, want 0", n)
	}

	// Contributors claim, out of band, with nothing telling us.
	chain.claimed[hex.EncodeToString(leafFor(ev, 0x07))] = true
	chain.claimed[hex.EncodeToString(leafFor(ev, 0x08))] = true

	if n, err := r.ReconcileClaims(context.Background(), ev); err != nil || n != 2 {
		t.Fatalf("polling pass promoted %d (err %v), want 2", n, err)
	}
	for _, b := range []byte{0x07, 0x08} {
		if got := stateOf(t, d, ev, b); got != chainops.StatePaid {
			t.Errorf("leaf %x = %s, want paid", b, got)
		}
	}
}

// An event with no published root yet is not a disagreement - there is simply
// nothing to reconcile against.
func TestReconcile_UnpublishedEventIsNotAMismatch(t *testing.T) {
	d := dbtest.DB(t)
	ev := event(t, d, leaf(0xEE), map[byte]chainops.State{0x09: chainops.StateConfirmed})

	chain := &fakeChain{pub: false, claimed: map[string]bool{}}
	alerter := &fakeAlerter{}
	r := reconciler.New(d.Pool, chain, chain, alerter)

	if _, err := r.ReconcileClaims(context.Background(), ev); err != nil {
		t.Fatalf("unpublished event errored: %v", err)
	}
	if len(alerter.sent) != 0 {
		t.Errorf("alerts = %v, want none", alerter.sent)
	}
}
