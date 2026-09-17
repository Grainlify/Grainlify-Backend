package keeperhub

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Reconciliation: matching what was dispatched to what came back, per leg.
//
// # The failure mode this exists to remove
//
// A caller holding only Execution.Legs sees the legs that reported and nothing
// about the ones that did not. A run where three legs were sent and one has no
// result looks, leg by leg, exactly like a smaller run that fully succeeded - and
// because only unpaid legs are ever re-sent, a leg that silently vanished from
// the result is a leg nobody retries and nobody pays. So a dispatched leg with
// no result is a typed, hard error here, never an absence.

// LegStatus is what a reconciled leg is KNOWN to be.
type LegStatus string

const (
	// LegConfirmed: every body step succeeded and a transaction was reported.
	LegConfirmed LegStatus = "confirmed"

	// LegFailed: a step failed and no transaction was reported, so no money
	// moved. Retrying it is correct.
	LegFailed LegStatus = "failed"

	// LegUnknown: we cannot say. Either a step failed after a transaction was
	// broadcast, or a step claimed success with no transaction to show for it.
	// Retrying an unknown leg is how a double payment happens; it is resolved by
	// reading the chain, by a person, before anything re-sends it.
	LegUnknown LegStatus = "unknown"
)

// LegOutcome is one dispatched leg matched to its result.
//
// Address and AmountMinor are copied from the DISPATCHED recipient, never from
// anything the workflow reported. In particular the transfer step's `amount`
// output is the human-readable derived value, and a leg's recorded amount must
// never come from it: the integer is the one that is stored, reconciled and
// audited.
type LegOutcome struct {
	LegID          string
	IterationIndex int
	Address        string
	AmountMinor    string
	Status         LegStatus
	TxHash         string
	Error          string
}

var (
	ErrExecutionNotTerminal   = errors.New("keeperhub: execution has not finished")
	ErrLegsWithoutResult      = errors.New("keeperhub: dispatched legs have no result")
	ErrExecutionInputMismatch = errors.New("keeperhub: execution input is not what was dispatched")
	ErrDuplicateLegID         = errors.New("keeperhub: legIds must be present and unique")
)

// MissingResultsError names the legs that were dispatched and came back with
// nothing. errors.Is(err, ErrLegsWithoutResult) matches it.
//
// It is returned ALONGSIDE the outcomes that did report. Failing closed on one
// leg must not discard what is known about the others: a settled leg whose
// transaction we hold should still be recorded as settled.
type MissingResultsError struct {
	ExecutionID string
	LegIDs      []string
}

func (e *MissingResultsError) Error() string {
	return fmt.Sprintf("%v: execution %s returned no result for %s - treat these legs as "+
		"unknown and resolve them before any resume",
		ErrLegsWithoutResult, e.ExecutionID, strings.Join(e.LegIDs, ", "))
}

func (e *MissingResultsError) Unwrap() error { return ErrLegsWithoutResult }

// Reconcile matches dispatched legs to a FINISHED execution's per-leg results.
//
// Mapping is by position, which is what the For Each guarantees: the dispatched
// array order is the iteration order, so iteration i belongs to dispatched[i],
// and its legId comes from there. Never by address - two runs, or two legs, can
// share an address, and matching money on a string is how the wrong row gets
// marked paid.
//
// The position mapping is only sound if the execution was actually given what
// we sent, so that is checked first against the execution's own recorded input.
// A mismatch is exactly what an idempotency replay looks like from the reading
// side: a resume deduped onto the original run would otherwise have the
// original run's first leg credited to the resume's first leg.
func Reconcile(dispatched []Recipient, ex Execution) ([]LegOutcome, error) {
	if len(dispatched) == 0 {
		return nil, ErrNothingToDispatch
	}
	seen := make(map[string]bool, len(dispatched))
	for i, d := range dispatched {
		if strings.TrimSpace(d.LegID) == "" {
			return nil, fmt.Errorf("%w: dispatched leg %d has no legId", ErrDuplicateLegID, i)
		}
		if seen[d.LegID] {
			return nil, fmt.Errorf("%w: %q appears twice", ErrDuplicateLegID, d.LegID)
		}
		seen[d.LegID] = true
	}

	// A missing result on a running execution means "not yet", not "never".
	if !ex.Terminal() {
		return nil, fmt.Errorf("%w: execution %s is %q", ErrExecutionNotTerminal, ex.ID, ex.Status)
	}

	if err := sameInput(dispatched, ex.Input); err != nil {
		return nil, fmt.Errorf("%w: execution %s: %v", ErrExecutionInputMismatch, ex.ID, err)
	}

	// Group every body node's log by iteration. An iteration has as many
	// entries as the loop body has nodes - the payout workflow has two, the
	// conversion and the transfer - so one entry per leg is not an assumption
	// this may make.
	byIteration := map[int][]LegResult{}
	for _, l := range ex.Legs {
		if l.IterationIndex < 0 || l.IterationIndex >= len(dispatched) {
			return nil, fmt.Errorf("%w: execution %s reports iteration %d for a dispatch of %d leg(s)",
				ErrExecutionInputMismatch, ex.ID, l.IterationIndex, len(dispatched))
		}
		byIteration[l.IterationIndex] = append(byIteration[l.IterationIndex], l)
	}

	var (
		out     []LegOutcome
		missing []string
	)
	for i, d := range dispatched {
		steps, ran := byIteration[i]
		if !ran {
			missing = append(missing, d.LegID)
			continue
		}
		o := LegOutcome{
			LegID:          d.LegID,
			IterationIndex: i,
			Address:        d.Address,
			AmountMinor:    d.AmountMinor,
		}
		o.Status, o.TxHash, o.Error = classify(steps)
		out = append(out, o)
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return out, &MissingResultsError{ExecutionID: ex.ID, LegIDs: missing}
	}
	return out, nil
}

// classify turns one iteration's steps into a leg status.
//
// The rule is "evidence of a broadcast decides", not "the step status decides":
//
//	any step failed, no tx       -> failed   (nothing moved; safe to retry)
//	any step failed, a tx exists -> unknown  (something may have moved)
//	all succeeded, one tx        -> confirmed
//	all succeeded, no tx         -> unknown  (a success claim with no evidence)
//	more than one distinct tx    -> unknown  (one leg is one transfer)
func classify(steps []LegResult) (LegStatus, string, string) {
	var (
		failed  bool
		errMsg  string
		hashes  = map[string]bool{}
		oneHash string
	)
	for _, s := range steps {
		if s.Status != "success" {
			failed = true
			if errMsg == "" {
				errMsg = s.Error
			}
			if errMsg == "" {
				errMsg = fmt.Sprintf("step %s reported %q", s.NodeID, s.Status)
			}
		}
		if s.TxHash != "" {
			hashes[s.TxHash] = true
			oneHash = s.TxHash
		}
	}

	switch {
	case len(hashes) > 1:
		return LegUnknown, "", "more than one transaction reported for a single leg"
	case failed && len(hashes) == 1:
		return LegUnknown, oneHash, "a step failed after a transaction was reported: " + errMsg
	case failed:
		return LegFailed, "", errMsg
	case len(hashes) == 1:
		return LegConfirmed, oneHash, ""
	default:
		return LegUnknown, "", "every step reported success but no transaction was recorded"
	}
}

// sameInput checks the execution was given exactly what was dispatched, in order.
//
// Addresses compare case-insensitively because EIP-55 gives one address two
// spellings. Amounts compare as exact strings, since both sides are the same
// integer serialised by us.
func sameInput(dispatched, got []Recipient) error {
	if len(got) != len(dispatched) {
		return fmt.Errorf("dispatched %d leg(s), execution recorded %d", len(dispatched), len(got))
	}
	for i := range dispatched {
		d, g := dispatched[i], got[i]
		if d.LegID != g.LegID {
			return fmt.Errorf("position %d: dispatched legId %q, execution has %q", i, d.LegID, g.LegID)
		}
		if !strings.EqualFold(d.Address, g.Address) {
			return fmt.Errorf("position %d (%s): address differs", i, d.LegID)
		}
		if d.AmountMinor != g.AmountMinor {
			return fmt.Errorf("position %d (%s): amountMinor %q vs %q", i, d.LegID, d.AmountMinor, g.AmountMinor)
		}
	}
	return nil
}
