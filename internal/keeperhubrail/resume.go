package keeperhubrail

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
)

// THE ONE DEFINITION of what a resume may send and what stops it.
//
// Release uses these to decide what to dispatch, and RunView uses the same
// functions to tell the admin screen what a release would do. They are shared
// rather than restated because two copies drift, and a screen that offers a
// send the backend refuses - or hides one it would make - is worse than no
// screen.

// A leg in a blocking status stops every resume: re-sending it could pay twice.
// A leg in a sendable status is what a resume sends. The two lists are
// disjoint, and 'confirmed' is in neither: it is final.
var (
	blockingStatuses = []string{"dispatched", "unknown"}
	sendableStatuses = []string{"pending", "failed"}
)

// Machine-readable reasons a leg blocks a resume.
const (
	// BlockAwaitingResult: sent, and its result has not been read back yet.
	BlockAwaitingResult = "awaiting_result"
	// BlockMayHavePaid: the outcome is unknown and money may have moved. It is
	// resolved against the chain by a person, never by re-sending.
	BlockMayHavePaid = "may_have_paid"
)

// LegBlockReason returns why a leg in this status blocks a resume, or "" when it
// does not.
func LegBlockReason(status string) string {
	switch status {
	case "dispatched":
		return BlockAwaitingResult
	case "unknown":
		return BlockMayHavePaid
	default:
		return ""
	}
}

// LegBlocks reports whether a leg in this status blocks every resume.
func LegBlocks(status string) bool { return slices.Contains(blockingStatuses, status) }

// LegResendable reports whether a leg in this status is sent by a resume.
//
// 'failed' is resendable because no money moved. 'unknown' is NOT: it may have
// paid.
func LegResendable(status string) bool { return slices.Contains(sendableStatuses, status) }

// querier is what both a pool and a transaction offer. Release reads inside its
// transaction; the read path reads from the pool. Same queries either way.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// sendableLeg is one leg a resume would dispatch.
type sendableLeg struct {
	ID          uuid.UUID
	Address     string
	AmountMinor string
}

// resumeState is everything about a run that decides whether a resume may
// send, and what.
type resumeState struct {
	RunID     uuid.UUID
	RunFailed bool
	Blocking  []uuid.UUID
	// Sendable is in dispatch order: user id, which is the order release has
	// always used and the order the workflow iterates.
	Sendable []sendableLeg
}

func loadResumeState(ctx context.Context, q querier, runID uuid.UUID, runState string) (*resumeState, error) {
	st := &resumeState{RunID: runID, RunFailed: runState == "failed"}

	rows, err := q.Query(ctx, `
		SELECT id FROM keeperhub_payout_legs
		WHERE run_id = $1 AND status = ANY($2)
		ORDER BY id`, runID, blockingStatuses)
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: blocking legs: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		st.Blocking = append(st.Blocking, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `
		SELECT id, address, amount_minor::text FROM keeperhub_payout_legs
		WHERE run_id = $1 AND status = ANY($2)
		ORDER BY user_id, id`, runID, sendableStatuses)
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: sendable legs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l sendableLeg
		if err := rows.Scan(&l.ID, &l.Address, &l.AmountMinor); err != nil {
			return nil, err
		}
		st.Sendable = append(st.Sendable, l)
	}
	return st, rows.Err()
}

// refusal is the run-level answer to "may a resume send now", as the error
// release returns. nil means it may. The order is release's order.
func (st *resumeState) refusal() error {
	switch {
	case st.RunFailed:
		// A failed run paid somewhere it should not have. Nothing more is sent
		// under it until a person has dealt with that.
		return fmt.Errorf("%w: run %s", ErrRunFailed, st.RunID)
	case len(st.Blocking) > 0:
		// A leg that is dispatched or unknown may have paid. Re-sending it is
		// the double payment this rail exists to prevent.
		return &UnreconciledLegsError{LegIDs: st.Blocking}
	case len(st.Sendable) == 0:
		return ErrNothingUnpaid
	default:
		return nil
	}
}

// checkEvent is every event-level precondition release checks before it
// touches a run: the release guard, the pool, that the payout run is the
// current computation, that the chain is an enabled EVM chain, and that the
// event is not already settled on Aptos. All of it is knowable without calling
// KeeperHub.
func (s *Service) checkEvent(ctx context.Context, req ReleaseRequest) error {
	// FIRST, before anything is read or written: the single chokepoint every
	// payout release must pass - explicit confirmation, an actor, not shadow
	// mode, phase settled, appeal window closed.
	if err := hackathon.GuardPayoutRelease(ctx, s.Pool, hackathon.PayoutReleaseRequest{
		HackathonID: req.HackathonID,
		PayoutRunID: req.PayoutRunID,
		ActorID:     req.ActorID,
		Confirm:     req.Confirm,
	}); err != nil {
		return err
	}

	if req.Pool != PoolContributor {
		return fmt.Errorf("%w (got %q)", ErrUnsupportedPool, req.Pool)
	}

	// The guard confirms a payout run id was supplied, not that it is the one
	// to pay. An upheld appeal recomputes into a NEW run, and paying the
	// superseded one pays the pre-appeal unit value - correct for nobody.
	var current uuid.UUID
	if err := s.Pool.QueryRow(ctx, `
		SELECT id FROM hackathon_payout_runs WHERE hackathon_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, req.HackathonID).Scan(&current); err != nil {
		return fmt.Errorf("%w: no computed payout run: %v", ErrPayoutRunNotCurrent, err)
	}
	if current != req.PayoutRunID {
		return fmt.Errorf("%w: asked to pay %s, current is %s", ErrPayoutRunNotCurrent, req.PayoutRunID, current)
	}

	family, err := payout.ChainFamilyFor(ctx, s.Pool, req.ChainID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotEVMChain, err)
	}
	if family != payout.FamilyEVM {
		return fmt.Errorf("%w: %q is %s", ErrNotEVMChain, req.ChainID, family)
	}

	// The Go-side half of the rail exclusion; the trigger from migration 090300
	// is the half that holds even for writers that skip this.
	if id, _ := hackathon.ExistingSettlementID(ctx, s.Pool, req.HackathonID, req.Pool); id != nil {
		return fmt.Errorf("%w: settlement %s", ErrSettledOnAptos, id)
	}
	return nil
}

// Refusal reasons, as the admin screen receives them. Each is the same name the
// release endpoint answers with for the same refusal, so a resume box and a
// refused release never disagree about why.
const (
	ReasonNotConfigured       = "keeperhub_not_configured"
	ReasonPayoutNotReleasable = "payout_not_releasable"
	ReasonPoolUnsupported     = "pool_unsupported"
	ReasonPayoutRunNotCurrent = "payout_run_not_current"
	ReasonChainNotEVM         = "chain_not_evm"
	ReasonSettledOnAptos      = "settled_on_aptos_rail"
	ReasonRunFailed           = "run_failed"
	ReasonUnreconciledLegs    = "unreconciled_legs"
	ReasonNothingUnpaid       = "nothing_unpaid"
	ReasonWorkflowDisabled    = "keeperhub_workflow_disabled"
	ReasonUnclassifiedRefusal = "refused"
)

// RefusalReason names a release refusal. The handler uses it too.
func RefusalReason(err error) string {
	var unreconciled *UnreconciledLegsError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, hackathon.ErrPayoutNotReleasable):
		return ReasonPayoutNotReleasable
	case errors.Is(err, ErrUnsupportedPool):
		return ReasonPoolUnsupported
	case errors.Is(err, ErrPayoutRunNotCurrent):
		return ReasonPayoutRunNotCurrent
	case errors.Is(err, ErrNotEVMChain):
		return ReasonChainNotEVM
	case errors.Is(err, ErrSettledOnAptos):
		return ReasonSettledOnAptos
	case errors.Is(err, ErrRunFailed):
		return ReasonRunFailed
	case errors.As(err, &unreconciled):
		return ReasonUnreconciledLegs
	case errors.Is(err, ErrNothingUnpaid):
		return ReasonNothingUnpaid
	case errors.Is(err, keeperhub.ErrWorkflowDisabled):
		return ReasonWorkflowDisabled
	default:
		return ReasonUnclassifiedRefusal
	}
}
