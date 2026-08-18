// Package chainops is the record of every on-chain operation this system
// attempts or observes, and the type boundary that keeps the two apart.
//
// # Two lifecycles, and why the distinction is load-bearing
//
// chain_operations holds rows that look identical and are not:
//
//   - fund, publish_root, sweep_residue, sweep_unclaimed — **we** submit these.
//     The row is written before submission as a record of intent, and a crashed
//     attempt is resumed by retrying it.
//
//   - claim — **the contributor** submits this, signing with their own key and
//     paying their own gas. The row is a record of observation, written when the
//     reconciler first sees the claim on chain.
//
// A reconciler that picked up a stale claim row and "recovered" it would be
// attempting to move funds to somebody who did not sign for it. That is a push
// payout: the one architecture this system forbids, and the reason claims are
// pull-based at all. It would arrive in review looking like careful
// crash-recovery logic, because that is precisely what it would be — applied to
// the wrong lifecycle.
//
// # Why this is a type and not a rule
//
// Branching on Kind inside the reconciler would work, and would rely on every
// future author noticing the distinction. Two things make the mistake impossible
// instead:
//
//  1. A database constraint. A claim row may only exist in an observed state
//     ('confirmed', 'paid', 'reorged'), so the query that finds retryable work —
//     state IN ('built','submitted') — **cannot return a claim row**, because
//     such a row cannot be stored. See migration 000077.
//
//  2. This package. Anything that submits or resubmits takes a Submittable,
//     which cannot be constructed from a claim row and cannot be constructed
//     outside this package at all.
//
// Either alone would be reasonable. Both means the guarantee survives somebody
// writing new query code that bypasses the helpers here, and also survives
// somebody restoring a database without the constraint.
package chainops

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Kind is what an operation does. The set is closed: chain_operations has no
// CHECK on kind, so this type is where the vocabulary is defined.
type Kind string

const (
	KindFund           Kind = "fund"
	KindPublishRoot    Kind = "publish_root"
	KindSweepResidue   Kind = "sweep_residue"
	KindSweepUnclaimed Kind = "sweep_unclaimed"
	// KindClaim is observed, never submitted. See the package comment.
	KindClaim Kind = "claim"
)

// State is where an operation has got to.
//
// StatePaid is deliberately distinct from StateConfirmed: confirmed means the
// transaction is on chain, paid means the reconciler read chain state and the
// leaf is settled. **Only the reconciler writes StatePaid**, and only after
// reading the chain.
type State string

const (
	StateBuilt     State = "built"
	StateSubmitted State = "submitted"
	StateConfirmed State = "confirmed"
	StatePaid      State = "paid"
	StateFailed    State = "failed"
	StateReorged   State = "reorged"
)

// Op is one row of chain_operations.
type Op struct {
	ID       uuid.UUID
	ChainID  string
	Kind     Kind
	EventRef uuid.UUID
	// LeafHash is set if and only if Kind is KindClaim, mirroring the database
	// constraint of the same shape.
	LeafHash []byte
	State    State
	TxHash   string
	Summary  string
}

// Observed reports whether this kind is something we watch rather than send.
func (k Kind) Observed() bool { return k == KindClaim }

var (
	// ErrNotSubmittable is returned when something tries to submit or retry an
	// operation we do not send. Today that means a claim.
	ErrNotSubmittable = errors.New("this operation is observed, not submitted")
	// ErrEmptySubmittable guards the zero value. A Submittable{} declared
	// outside this package has no Op inside it, and must not be mistaken for a
	// validated one.
	ErrEmptySubmittable = errors.New("submittable was not produced by AsSubmittable")
)

// Submittable is an operation this system is permitted to send to a chain.
//
// The field is unexported and a pointer, so the type cannot be built anywhere
// except AsSubmittable, and its zero value carries nothing. That is the whole
// mechanism: a function taking a Submittable cannot be handed a claim, because
// no route exists to produce one.
type Submittable struct {
	op *Op
}

// AsSubmittable is the single gate. It is the only place in the codebase where
// "may we send this?" is decided, which is what makes the answer reviewable.
func AsSubmittable(op Op) (Submittable, error) {
	if op.Kind.Observed() {
		return Submittable{}, fmt.Errorf(
			"%w: %s operations are submitted by the contributor, and retrying one "+
				"would push funds to an address that did not sign for them",
			ErrNotSubmittable, op.Kind)
	}
	if op.Kind == "" {
		return Submittable{}, fmt.Errorf("%w: operation has no kind", ErrNotSubmittable)
	}
	// Go already copies `op` - it is a value parameter - so taking its address
	// cannot alias the caller's variable. What a struct copy does NOT do is
	// duplicate a slice's backing array, so LeafHash is cloned explicitly.
	//
	// Low stakes today, since only claims carry a leaf hash and claims never get
	// this far. Done anyway because the alternative is a validated value whose
	// contents somebody else can still change, which is not a thing worth having
	// in the path that decides whether we may move money.
	cp := op
	if op.LeafHash != nil {
		cp.LeafHash = append([]byte(nil), op.LeafHash...)
	}
	return Submittable{op: &cp}, nil
}

// Op returns the underlying operation. The LeafHash is this Submittable's own
// copy, so mutating it cannot reach back into whatever produced it.
//
// Returns an error rather than a zero Op for the un-constructed case, so a
// caller that ignored AsSubmittable's error cannot proceed with an empty
// operation and a nil check it forgot to write.
func (s Submittable) Op() (Op, error) {
	if s.op == nil {
		return Op{}, ErrEmptySubmittable
	}
	return *s.op, nil
}

// PendingStates are the states the reconciler treats as unfinished work.
//
// By the database constraint added in migration 000077, a claim row can never
// hold either of these — so a query filtered on them returns only operations we
// are allowed to resubmit. The safety here is in the data, not in the caller
// remembering to filter by kind as well.
func PendingStates() []State { return []State{StateBuilt, StateSubmitted} }
