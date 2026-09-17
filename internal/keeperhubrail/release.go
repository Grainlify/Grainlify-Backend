package keeperhubrail

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// ReleaseRequest is the explicit admin action that sends money.
type ReleaseRequest struct {
	HackathonID uuid.UUID
	PayoutRunID uuid.UUID
	ActorID     uuid.UUID
	Pool        string
	ChainID     string
	Confirm     bool
}

// ReleaseResult describes what was SENT. It says nothing about what was paid.
type ReleaseResult struct {
	RunID       uuid.UUID   `json:"run_id"`
	Planned     bool        `json:"planned"`
	AttemptID   uuid.UUID   `json:"attempt_id"`
	ExecutionID string      `json:"execution_id"`
	LegIDs      []uuid.UUID `json:"dispatched_leg_ids"`
	Exclusions  []Exclusion `json:"exclusions"`

	// AckStatus is KeeperHub's acknowledgement, verbatim - normally
	// "running". It is ACCEPTANCE, not payment: the probe run finished after
	// its acknowledgement returned, with nothing in the envelope saying so.
	// Payment is established only by Intake.
	AckStatus string `json:"ack_status"`
}

// Release dispatches every unpaid leg of an event's run, planning the run first
// if it does not exist yet.
func (s *Service) Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error) {
	// Every event-level precondition, in release's order. Shared with RunView
	// so the admin screen and this function cannot disagree (see resume.go).
	if err := s.checkEvent(ctx, req); err != nil {
		return nil, err
	}

	attemptID, recipients, legIDs, run, planned, exclusions, err := s.claimUnpaidLegs(ctx, req)
	if err != nil {
		return nil, err
	}

	// The legs are ALREADY recorded as dispatched, in a committed transaction,
	// before this call. If the process dies mid-request they read as
	// dispatched rather than pending, so nothing can send them a second time.
	ack, dispatchErr := s.Rail.Dispatch(ctx, recipients)
	if dispatchErr != nil {
		// Only a failure the client CLASSIFIED as a definitive rejection makes
		// the legs resumable. Anything else - including an error the client
		// did not produce - leaves them unknown.
		if keeperhub.IsRejected(dispatchErr) {
			s.markRejected(ctx, attemptID, ack, dispatchErr)
			return nil, fmt.Errorf("%w: attempt %s: %w", ErrDispatchRejected, attemptID, dispatchErr)
		}
		s.markUnacknowledged(ctx, attemptID, ack.IdempotencyKey, dispatchErr)
		return nil, fmt.Errorf("%w: attempt %s: %w", ErrDispatchUnknown, attemptID, dispatchErr)
	}

	if err := s.markSent(ctx, attemptID, ack); err != nil {
		// Money may be moving and we failed to write down which execution is
		// doing it. Say so loudly: the attempt is left "sending", its legs are
		// dispatched and therefore blocked, and the execution id is here.
		return nil, fmt.Errorf("keeperhubrail: dispatched as execution %s (key %s) but could not record it: %w",
			ack.ExecutionID, ack.IdempotencyKey, err)
	}

	return &ReleaseResult{
		RunID:       run,
		Planned:     planned,
		AttemptID:   attemptID,
		ExecutionID: ack.ExecutionID,
		LegIDs:      legIDs,
		Exclusions:  exclusions,
		AckStatus:   ack.Status,
	}, nil
}

// claimUnpaidLegs plans the run if needed, refuses while anything is
// unreconciled, and marks the unpaid legs dispatched under a new attempt - all
// in one transaction.
func (s *Service) claimUnpaidLegs(ctx context.Context, req ReleaseRequest) (
	attemptID uuid.UUID, recipients []keeperhub.Recipient, legIDs []uuid.UUID,
	runID uuid.UUID, planned bool, exclusions []Exclusion, err error,
) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)

	run, err := loadRun(ctx, tx, req.HackathonID, req.Pool, true)
	if err != nil {
		return
	}
	if run == nil {
		run, exclusions, err = s.planRun(ctx, tx, req)
		if err != nil {
			return
		}
		planned = true
	} else {
		if run.ChainID != req.ChainID || run.PayoutRunID != req.PayoutRunID {
			err = fmt.Errorf("%w: run %s pays chain %q from computation %s", ErrRunMismatch,
				run.ID, run.ChainID, run.PayoutRunID)
			return
		}
		if exclusions, err = exclusionsFor(ctx, tx, run.ID); err != nil {
			return
		}
	}
	runID = run.ID

	// THE RESUME GATE, from the same definition RunView reports (resume.go):
	// a failed run, any dispatched or unknown leg, or nothing to send refuses.
	st, err := loadResumeState(ctx, tx, run.ID, run.State)
	if err != nil {
		return
	}
	for _, l := range st.Sendable {
		legIDs = append(legIDs, l.ID)
		recipients = append(recipients, keeperhub.Recipient{Address: l.Address, AmountMinor: l.AmountMinor, LegID: l.ID.String()})
	}
	if refusal := st.refusal(); refusal != nil {
		// Commit anyway when the run was just planned and nothing is
		// sendable: everyone may have been excluded, and those exclusions are
		// a record worth keeping.
		if planned && errors.Is(refusal, ErrNothingUnpaid) {
			if cerr := tx.Commit(ctx); cerr != nil {
				err = cerr
				return
			}
		}
		err = refusal
		return
	}

	if err = tx.QueryRow(ctx, `
		INSERT INTO keeperhub_dispatch_attempts (run_id, actor_user_id, state)
		VALUES ($1, $2, 'sending') RETURNING id`, run.ID, req.ActorID).Scan(&attemptID); err != nil {
		return
	}
	for i, id := range legIDs {
		if _, err = tx.Exec(ctx, `
			INSERT INTO keeperhub_dispatch_attempt_legs (attempt_id, leg_id, position)
			VALUES ($1, $2, $3)`, attemptID, id, i); err != nil {
			return
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs
		SET status = 'dispatched', last_attempt_id = $1, dispatched_at = now(),
		    execution_id = NULL, last_error = NULL, updated_at = now()
		WHERE id = ANY($2) AND status = ANY($3)`, attemptID, legIDs, sendableStatuses)
	if err != nil {
		return
	}
	if int(tag.RowsAffected()) != len(legIDs) {
		err = ErrConcurrentRelease
		return
	}
	if _, err = tx.Exec(ctx, `
		UPDATE keeperhub_payout_runs SET state = 'dispatching', updated_at = now() WHERE id = $1`, run.ID); err != nil {
		return
	}
	err = tx.Commit(ctx)
	return
}

// markRejected records a dispatch KeeperHub definitively did not run, and puts
// its legs back to failed so the next release can send them.
//
// This is the whole reason dispatch failures are classified. Marking a 401 as
// unknown strands every leg behind a manual chain check for a condition we
// already know with certainty - nothing ran - and a safe path that expensive is
// one people learn to route around.
//
// A 402 carries an execution id for a run that was created and never started;
// it is recorded on the attempt for the audit trail, and does not make the
// legs anything other than failed.
func (s *Service) markRejected(ctx context.Context, attemptID uuid.UUID, ack keeperhub.DispatchAck, cause error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return // legs stay dispatched, which blocks a resume: safe
	}
	defer tx.Rollback(ctx)
	var keyArg, execArg any
	if ack.IdempotencyKey != "" {
		keyArg = ack.IdempotencyKey
	}
	if ack.ExecutionID != "" {
		execArg = ack.ExecutionID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_dispatch_attempts
		SET state = 'rejected', idempotency_key = $2, execution_id = $3, error = $4
		WHERE id = $1`, attemptID, keyArg, execArg, cause.Error()); err != nil {
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs
		SET status = 'failed', last_error = $2, updated_at = now()
		WHERE last_attempt_id = $1 AND status = 'dispatched'`, attemptID,
		"dispatch rejected, nothing sent: "+cause.Error()); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

// markUnacknowledged records a dispatch whose outcome is unknown.
//
// Used for everything the client did not classify as a definitive rejection:
// timeouts, 5xx, 408, 409 (the first request under the key may still be
// paying), an accepted request with no execution id, and anything unrecognised.
// Guessing wrong in the permissive direction is a double payment; guessing wrong
// in this direction costs a person a chain read.
func (s *Service) markUnacknowledged(ctx context.Context, attemptID uuid.UUID, key string, cause error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var keyArg any
	if key != "" {
		keyArg = key
	}
	_, _ = tx.Exec(ctx, `
		UPDATE keeperhub_dispatch_attempts
		SET state = 'unacknowledged', idempotency_key = $2, error = $3
		WHERE id = $1`, attemptID, keyArg, cause.Error())
	_, _ = tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs
		SET status = 'unknown', last_error = $2, updated_at = now()
		WHERE last_attempt_id = $1 AND status = 'dispatched'`, attemptID,
		"dispatch outcome unknown: "+cause.Error())
	_ = tx.Commit(ctx)
}

func (s *Service) markSent(ctx context.Context, attemptID uuid.UUID, ack keeperhub.DispatchAck) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_dispatch_attempts
		SET state = 'sent', execution_id = $2, idempotency_key = $3
		WHERE id = $1`, attemptID, ack.ExecutionID, ack.IdempotencyKey); err != nil {
		return err
	}
	// The execution id is recorded on the legs; their status stays dispatched.
	// An acknowledgement is not a payment.
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs SET execution_id = $2, updated_at = now()
		WHERE last_attempt_id = $1 AND status = 'dispatched'`, attemptID, ack.ExecutionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isNotFound is a small helper for scans scoped to a hackathon.
func isNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
