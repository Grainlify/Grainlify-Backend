package chainops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainops"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The Go gate in this package can be bypassed by anyone writing their own SQL.
// These tests assert the other half: that Postgres itself refuses to store the
// row that would make a push payout possible.
//
// Both layers matter for different reasons. The type survives a database
// restored without its constraints; the constraint survives new query code that
// never touches this package.

// TestSchema_AClaimCannotBeStoredInARetryableState is the constraint that makes
// the reconciler's query safe by construction.
//
// The reconciler finds work with `state IN ('built','submitted')`. If a claim
// row could hold either, that query would hand it a claim to "recover" - and
// recovering a claim means submitting a transfer to somebody who did not sign
// for it. Here the database refuses to record such a row at all, so the query
// cannot return one no matter how it is written.
func TestSchema_AClaimCannotBeStoredInARetryableState(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	for _, state := range []chainops.State{chainops.StateBuilt, chainops.StateSubmitted} {
		_, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref, leaf_hash)
VALUES ($1, 'claim', $2, $3, $4)
`, "aptos-testnet", string(state), uuid.New(), []byte("leaf"))

		if err == nil {
			t.Errorf("the database accepted a claim row in state %q; a reconciler "+
				"polling for retryable work could pick it up and attempt a push payout", state)
			continue
		}
		if !strings.Contains(err.Error(), "claims_are_observed_not_submitted") {
			t.Errorf("claim in state %q was rejected, but not by the intended "+
				"constraint: %v", state, err)
		}
	}
}

// TestSchema_AClaimIsAcceptedInEveryObservedState is the other side, and stops
// the constraint above being satisfied by a rule that simply forbids claims.
func TestSchema_AClaimIsAcceptedInEveryObservedState(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	for _, state := range []chainops.State{
		chainops.StateConfirmed, chainops.StatePaid, chainops.StateReorged,
	} {
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref, leaf_hash)
VALUES ($1, 'claim', $2, $3, $4)
`, "aptos-testnet", string(state), uuid.New(), []byte("leaf-"+state)); err != nil {
			t.Errorf("the database refused a claim row in observed state %q: %v", state, err)
		}
	}
}

// TestSchema_PaidIsAStateThatExists guards the distinction between "the
// transaction landed" and "this person has their money". Only the second is safe
// to stop retrying on.
func TestSchema_PaidIsAStateThatExists(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref)
VALUES ('aptos-testnet', 'publish_root', 'paid', $1)
`, uuid.New()); err != nil {
		t.Fatalf("state 'paid' was rejected; the reconciler has nothing to promote to: %v", err)
	}
}

// TestSchema_LeafHashBelongsToClaimsAndNothingElse keeps the two shapes
// distinct: a claim settles one leaf, everything else acts on a whole event.
func TestSchema_LeafHashBelongsToClaimsAndNothingElse(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	// A non-claim carrying a leaf hash.
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref, leaf_hash)
VALUES ('aptos-testnet', 'publish_root', 'built', $1, $2)
`, uuid.New(), []byte("leaf")); err == nil {
		t.Error("a publish_root row was allowed to carry a leaf_hash; it acts on a whole event")
	}

	// A claim without one.
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref)
VALUES ('aptos-testnet', 'claim', 'confirmed', $1)
`, uuid.New()); err == nil {
		t.Error("a claim row was allowed without a leaf_hash; it settles exactly one leaf")
	}
}

// TestSchema_OneAttemptPerOperation is what makes a retry safe: a process that
// crashes between writing the row and submitting must not be able to write a
// second row on restart.
func TestSchema_OneAttemptPerOperation(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	event := uuid.New()

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref)
VALUES ('aptos-testnet', 'publish_root', 'built', $1)
`, event); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO chain_operations (chain_id, kind, state, event_ref)
VALUES ('aptos-testnet', 'publish_root', 'built', $1)
`, event); err == nil {
		t.Error("a second publish_root attempt for the same event was accepted; " +
			"the reconciler would see two rows for one operation and could not tell " +
			"whether that meant one transaction or two")
	}
}
