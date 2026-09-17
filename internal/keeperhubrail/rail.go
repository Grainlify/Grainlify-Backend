// Package keeperhubrail releases a settled hackathon's contributor pool over
// KeeperHub, and takes the results back in.
//
// # The rules everything here serves
//
//   - THE LEG IS THE UNIT OF TRUTH. Only legs that are pending or failed are
//     ever dispatched, and a run is never re-fired as a whole. KeeperHub cannot
//     resume a partially failed For Each - re-running one re-pays settled legs,
//     which was proved on chain - so resumability is ours.
//   - A leg that MAY have paid blocks every resume until a person has
//     established whether it did. "dispatched" and "unknown" legs are never
//     re-sent, and never quietly moved back to pending.
//   - A dispatch acknowledgement is acceptance, never payment. Nothing here
//     marks a leg confirmed except per-leg results read back from the
//     execution, or a person resolving it with a transaction hash.
//   - hackathon.SettlementFor is used as a PURE PRODUCER. Nothing in this
//     package writes a settlements row: doing so would consume
//     settlements_one_per_event_pool and permanently block the Aptos rail for
//     the event.
package keeperhubrail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// Rail is the KeeperHub surface this package needs. *keeperhub.Client satisfies
// it; tests substitute a fake so the orchestration can be exercised without
// moving anything.
type Rail interface {
	Dispatch(ctx context.Context, recipients []keeperhub.Recipient) (keeperhub.DispatchAck, error)
	Execution(ctx context.Context, executionID string) (keeperhub.Execution, error)
}

// Service releases and reconciles KeeperHub payout runs.
type Service struct {
	Pool db.DBPool
	Rail Rail
}

// Pool kinds this rail pays. Only the contributor pool is settled from
// verdicts; SettlementFor refuses the maintainer pool, and so does this.
const PoolContributor = "contributor"

// One error per cause. Every one of these is an operator reading a refusal and
// needing to know which thing to go and fix.
var (
	ErrNotEVMChain          = errors.New("keeperhubrail: chain is not an enabled EVM chain")
	ErrUnsupportedPool      = errors.New("keeperhubrail: only the contributor pool is paid on this rail")
	ErrPayoutRunNotCurrent  = errors.New("keeperhubrail: payout run is not this hackathon's current computation")
	ErrSettledOnAptos       = errors.New("keeperhubrail: this event and pool is already settled on the Aptos rail")
	ErrRunMismatch          = errors.New("keeperhubrail: an existing run for this event pays a different chain or computation")
	ErrNothingUnpaid        = errors.New("keeperhubrail: no pending or failed legs to dispatch")
	ErrConcurrentRelease    = errors.New("keeperhubrail: another release changed these legs; nothing was sent")
	ErrDispatchUnknown      = errors.New("keeperhubrail: dispatch outcome unknown; its legs are now unknown")
	ErrDispatchRejected     = errors.New("keeperhubrail: KeeperHub rejected the dispatch and ran nothing; its legs are failed and resumable")
	ErrNoExecutionToRead    = errors.New("keeperhubrail: attempt has no execution to read")
	ErrNotFound             = errors.New("keeperhubrail: not found for this hackathon")
	ErrLegNotResolvable     = errors.New("keeperhubrail: only an unknown leg can be resolved by hand")
	ErrResolutionIncomplete = errors.New("keeperhubrail: a resolution needs a status, a note, and a tx hash when confirming")
	ErrAmountOutOfRange     = errors.New("keeperhubrail: leg amount does not fit a leg record")
	ErrAllocationMismatch   = errors.New("keeperhubrail: legs and exclusions do not sum to the pool")
)

// UnreconciledLegsError blocks a resume. errors.Is(err, ErrUnreconciled) matches.
//
// Carries the legs by id so the refusal is actionable: these are the legs a
// person has to establish on chain before anything else may be sent.
type UnreconciledLegsError struct {
	LegIDs []uuid.UUID
}

// ErrUnreconciled is the sentinel UnreconciledLegsError unwraps to.
var ErrUnreconciled = errors.New("keeperhubrail: legs that may have paid are not yet reconciled")

func (e *UnreconciledLegsError) Error() string {
	ids := make([]string, 0, len(e.LegIDs))
	for _, id := range e.LegIDs {
		ids = append(ids, id.String())
	}
	return fmt.Sprintf("%v: %s - read their results or resolve them against the chain "+
		"before any resume; re-sending a leg that already paid pays it twice",
		ErrUnreconciled, strings.Join(ids, ", "))
}

func (e *UnreconciledLegsError) Unwrap() error { return ErrUnreconciled }

// Exclusion is somebody who earned an amount and was not made a leg.
type Exclusion struct {
	UserID      uuid.UUID `json:"user_id"`
	AmountMinor string    `json:"amount_minor"`
	Reason      string    `json:"reason"`
}

// Exclusion reasons, in the order payout.Resolve applies them on the Aptos rail.
// Kept identical so a person is never payable on one rail and held on the other
// for the same event.
const (
	ReasonNoGitHubAccount = "no_github_account"
	ReasonKYCUnresolved   = "kyc_unresolved"
	ReasonNoAddress       = "no_address"
)
