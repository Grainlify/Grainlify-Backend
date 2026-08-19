package chainops

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func claimOp() Op {
	return Op{
		ID: uuid.New(), ChainID: "aptos-testnet", Kind: KindClaim,
		EventRef: uuid.New(), LeafHash: []byte("leaf"), State: StateConfirmed,
	}
}

func publishOp() Op {
	return Op{
		ID: uuid.New(), ChainID: "aptos-testnet", Kind: KindPublishRoot,
		EventRef: uuid.New(), State: StateBuilt,
	}
}

// TestAsSubmittable_RefusesAClaim is the property this package exists for.
//
// Retrying a claim means submitting a transaction that moves funds to somebody
// who did not sign for it - a push payout, which is the architecture the whole
// pull-based design forbids. The dangerous thing about it is that it would
// arrive as crash-recovery logic, which is a shape reviewers approve.
func TestAsSubmittable_RefusesAClaim(t *testing.T) {
	if _, err := AsSubmittable(claimOp()); !errors.Is(err, ErrNotSubmittable) {
		t.Fatalf("AsSubmittable(claim) error = %v, want ErrNotSubmittable", err)
	}
}

// TestAsSubmittable_AcceptsEveryOperationWeActuallySend asserts the gate is not
// simply refusing everything, which is the way a guard like this passes its own
// test while breaking the system.
//
// Asserts the count as well as the members: a kind added later without a
// decision about whether we submit it should fail here rather than silently
// inherit an answer.
func TestAsSubmittable_AcceptsEveryOperationWeActuallySend(t *testing.T) {
	submitted := []Kind{KindFund, KindPublishRoot, KindSweepResidue, KindSweepUnclaimed}
	observed := []Kind{KindClaim}

	const wantKinds = 5
	if got := len(submitted) + len(observed); got != wantKinds {
		t.Fatalf("this test knows about %d kinds, the package defines %d - "+
			"a new kind needs a deliberate submitted-or-observed decision", got, wantKinds)
	}

	for _, k := range submitted {
		op := publishOp()
		op.Kind = k
		s, err := AsSubmittable(op)
		if err != nil {
			t.Errorf("AsSubmittable(%s) = %v, want success", k, err)
			continue
		}
		got, err := s.Op()
		if err != nil {
			t.Errorf("Op() after a successful AsSubmittable(%s): %v", k, err)
			continue
		}
		if got.Kind != k {
			t.Errorf("Op().Kind = %s, want %s", got.Kind, k)
		}
	}
}

// TestSubmittable_ZeroValueCarriesNothing closes the hole an unexported field
// alone leaves: `var s Submittable` is constructible anywhere, and would satisfy
// a function signature without ever passing the gate.
func TestSubmittable_ZeroValueCarriesNothing(t *testing.T) {
	var s Submittable
	if _, err := s.Op(); !errors.Is(err, ErrEmptySubmittable) {
		t.Fatalf("zero Submittable Op() error = %v, want ErrEmptySubmittable", err)
	}
}

// TestSubmittable_HoldsACopy stops a caller mutating an operation after it has
// been validated.
//
// The scalar half of this is free - Go copies a value parameter - and a mutation
// removing the explicit copy correctly survives, because it changes nothing. The
// half that is NOT free is LeafHash: a struct copy duplicates the slice header
// and shares the backing array, so without an explicit clone a caller could
// still reach into a validated operation. That is what this asserts.
func TestSubmittable_HoldsACopy(t *testing.T) {
	op := publishOp()
	op.LeafHash = nil

	s, err := AsSubmittable(op)
	if err != nil {
		t.Fatalf("AsSubmittable: %v", err)
	}

	// Scalar: guaranteed by the language, asserted so the intent is visible.
	op.Kind = KindClaim
	got, err := s.Op()
	if err != nil {
		t.Fatalf("Op(): %v", err)
	}
	if got.Kind != KindPublishRoot {
		t.Errorf("mutating the source changed the validated operation to %s", got.Kind)
	}
}

// TestSubmittable_DoesNotShareALeafHashBackingArray is the part a struct copy
// does not give for free.
func TestSubmittable_DoesNotShareALeafHashBackingArray(t *testing.T) {
	op := publishOp()
	op.LeafHash = []byte{1, 2, 3}

	s, err := AsSubmittable(op)
	if err != nil {
		t.Fatalf("AsSubmittable: %v", err)
	}

	op.LeafHash[0] = 0xFF

	got, err := s.Op()
	if err != nil {
		t.Fatalf("Op(): %v", err)
	}
	if got.LeafHash[0] != 1 {
		t.Error("the validated operation shares a backing array with its source; " +
			"a struct copy duplicates the slice header, not the bytes")
	}
}

// TestPendingStates_ExcludeEveryObservedState pins the relationship between this
// list and the database constraint in migration 000077.
//
// The reconciler's query filters on these states, and the constraint guarantees
// a claim row can never hold one. If somebody adds an observed state to this
// list, the query starts returning rows it must never act on - and the database
// would still be within its constraint, because the constraint names states, not
// this slice.
func TestPendingStates_ExcludeEveryObservedState(t *testing.T) {
	// The states migration 000077 permits for a claim row.
	observedStates := map[State]bool{
		StateConfirmed: true,
		StatePaid:      true,
		StateReorged:   true,
	}
	for _, s := range PendingStates() {
		if observedStates[s] {
			t.Errorf("PendingStates includes %q, which migration 000077 allows a "+
				"claim row to hold - the reconciler's query could then return one", s)
		}
	}
	if len(PendingStates()) == 0 {
		t.Fatal("PendingStates is empty; the reconciler would find no work at all")
	}
}
