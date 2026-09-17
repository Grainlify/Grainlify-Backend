package keeperhubrail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// IntakeResult is what reading an attempt back established.
type IntakeResult struct {
	AttemptID   uuid.UUID `json:"attempt_id"`
	ExecutionID string    `json:"execution_id"`

	// Finished is false while the execution is still running. Nothing is
	// written in that case: a missing result on a running execution means "not
	// yet", and recording it as anything would be a guess.
	Finished bool `json:"finished"`

	// AlreadyReconciled is true when this attempt was read back before. Intake
	// is idempotent; a second call writes nothing.
	AlreadyReconciled bool `json:"already_reconciled"`

	Outcomes []keeperhub.LegOutcome `json:"outcomes"`

	// Unknown lists legs that are now unknown because the execution returned no
	// result for them, or because the execution was not what we sent.
	Unknown []string `json:"unknown_leg_ids"`

	RunComplete bool `json:"run_complete"`
}

// Intake reads an attempt's execution back and writes each leg's result.
//
// Results come from MCP get_execution, per iteration. Never from
// /api/analytics/runs, whose executionId filter is silently ignored, and never
// from the post-loop Collect, which does not run when any leg fails.
func (s *Service) Intake(ctx context.Context, hackathonID, attemptID uuid.UUID) (*IntakeResult, error) {
	var (
		state    string
		execID   *string
		runID    uuid.UUID
		runChain int64
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT a.state, a.execution_id, a.run_id, r.evm_chain_id
		FROM keeperhub_dispatch_attempts a
		JOIN keeperhub_payout_runs r ON r.id = a.run_id
		WHERE a.id = $1 AND r.hackathon_id = $2`, attemptID, hackathonID).Scan(&state, &execID, &runID, &runChain)
	if isNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: load attempt: %w", err)
	}

	out := &IntakeResult{AttemptID: attemptID}
	if execID != nil {
		out.ExecutionID = *execID
	}
	switch state {
	case "reconciled", "mismatch":
		out.Finished = true
		out.AlreadyReconciled = true
		return out, nil
	case "sent":
	default:
		// sending / unacknowledged: there is no execution id to read. Its legs
		// are unknown and must be resolved against the chain by a person.
		return out, fmt.Errorf("%w: attempt %s is %q", ErrNoExecutionToRead, attemptID, state)
	}

	dispatched, err := attemptRecipients(ctx, s.Pool, attemptID)
	if err != nil {
		return nil, err
	}

	ex, err := s.Rail.Execution(ctx, out.ExecutionID)
	if err != nil {
		return nil, err
	}
	if !ex.Terminal() {
		return out, nil
	}
	out.Finished = true

	outcomes, recErr := keeperhub.Reconcile(dispatched, ex)
	var missing *keeperhub.MissingResultsError
	switch {
	case recErr == nil:
	case errors.As(recErr, &missing):
		// Record what did report; the rest become unknown below.
	case errors.Is(recErr, keeperhub.ErrExecutionInputMismatch):
		// Not our run's results. Every leg of the attempt is unknown: money may
		// have moved under an execution we cannot attribute.
		for _, d := range dispatched {
			out.Unknown = append(out.Unknown, d.LegID)
		}
		if err := s.writeIntake(ctx, attemptID, runID, "mismatch", nil, out.Unknown, recErr.Error()); err != nil {
			return nil, err
		}
		return out, recErr
	default:
		return nil, recErr
	}

	out.Outcomes = outcomes
	if missing != nil {
		out.Unknown = missing.LegIDs
	}

	// CHAIN IDENTITY. Every leg with a transaction is checked against the
	// numeric chain frozen on the run, using the chainId KeeperHub reports for
	// that transaction - what happened, not what was configured. One mismatch
	// fails the whole run closed: a transfer on the wrong network is not a
	// payment on this one, and the other legs of an execution that reached a
	// different network cannot be trusted either.
	var wrong []string
	for _, o := range outcomes {
		if o.TxHash != "" && o.ChainID != runChain {
			wrong = append(wrong, fmt.Sprintf("%s: tx %s on chain %d", o.LegID, o.TxHash, o.ChainID))
		}
	}
	if len(wrong) > 0 {
		reason := fmt.Sprintf("chain mismatch: the run pays on chain %d, but %s", runChain, strings.Join(wrong, "; "))
		var all []string
		for _, d := range dispatched {
			all = append(all, d.LegID)
		}
		out.Outcomes = nil
		out.Unknown = all
		if err := s.failRunOnChainMismatch(ctx, attemptID, runID, reason); err != nil {
			return nil, err
		}
		return out, fmt.Errorf("%w: %s", ErrChainMismatch, reason)
	}
	if err := s.writeIntake(ctx, attemptID, runID, "reconciled", outcomes, out.Unknown,
		"no result in execution "+out.ExecutionID); err != nil {
		return nil, err
	}
	out.RunComplete, err = s.runComplete(ctx, runID)
	if err != nil {
		return nil, err
	}
	if missing != nil {
		// Returned alongside the written result: the caller must see that some
		// legs are now blocking, not a clean success.
		return out, missing
	}
	return out, nil
}

func attemptRecipients(ctx context.Context, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, attemptID uuid.UUID) ([]keeperhub.Recipient, error) {
	rows, err := pool.Query(ctx, `
		SELECT l.id, l.address, l.amount_minor::text
		FROM keeperhub_dispatch_attempt_legs al
		JOIN keeperhub_payout_legs l ON l.id = al.leg_id
		WHERE al.attempt_id = $1
		ORDER BY al.position`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: load attempt legs: %w", err)
	}
	defer rows.Close()
	var out []keeperhub.Recipient
	for rows.Next() {
		var id uuid.UUID
		var r keeperhub.Recipient
		if err := rows.Scan(&id, &r.Address, &r.AmountMinor); err != nil {
			return nil, err
		}
		r.LegID = id.String()
		out = append(out, r)
	}
	return out, rows.Err()
}

// writeIntake records outcomes and unknowns for one attempt.
//
// Only legs still "dispatched" under THIS attempt are touched, so reading an old
// attempt late can never overwrite what a newer one, or a person, established.
func (s *Service) writeIntake(ctx context.Context, attemptID, runID uuid.UUID, attemptState string,
	outcomes []keeperhub.LegOutcome, unknown []string, unknownReason string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, o := range outcomes {
		var tx_ any
		if o.TxHash != "" {
			tx_ = o.TxHash
		}
		var lastErr any
		if o.Error != "" {
			lastErr = o.Error
		}
		if _, err := tx.Exec(ctx, `
			UPDATE keeperhub_payout_legs
			SET status = $3, tx_hash = $4, last_error = $5,
			    confirmed_at = CASE WHEN $3 = 'confirmed' THEN now() ELSE NULL END,
			    updated_at = now()
			WHERE id = $1 AND last_attempt_id = $2 AND status = 'dispatched'`,
			o.LegID, attemptID, string(o.Status), tx_, lastErr); err != nil {
			return fmt.Errorf("keeperhubrail: write leg %s: %w", o.LegID, err)
		}
	}
	for _, id := range unknown {
		if _, err := tx.Exec(ctx, `
			UPDATE keeperhub_payout_legs
			SET status = 'unknown', last_error = $3, updated_at = now()
			WHERE id = $1 AND last_attempt_id = $2 AND status = 'dispatched'`,
			id, attemptID, unknownReason); err != nil {
			return fmt.Errorf("keeperhubrail: mark leg %s unknown: %w", id, err)
		}
	}
	errText := any(nil)
	if attemptState == "mismatch" {
		errText = unknownReason
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_dispatch_attempts
		SET state = $2, reconciled_at = now(), error = COALESCE($3, error)
		WHERE id = $1`, attemptID, attemptState, errText); err != nil {
		return err
	}
	if err := setRunState(ctx, tx, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// failRunOnChainMismatch records no leg as confirmed, marks every leg of the
// attempt unknown, and fails the run so nothing more is sent under it.
func (s *Service) failRunOnChainMismatch(ctx context.Context, attemptID, runID uuid.UUID, reason string) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs SET status = 'unknown', last_error = $2, updated_at = now()
		WHERE last_attempt_id = $1 AND status = 'dispatched'`, attemptID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_dispatch_attempts SET state = 'mismatch', reconciled_at = now(), error = $2
		WHERE id = $1`, attemptID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_runs SET state = 'failed', updated_at = now() WHERE id = $1`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setRunState marks a run complete only when EVERY leg is confirmed.
func setRunState(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_runs r
		SET state = CASE
		      WHEN EXISTS (SELECT 1 FROM keeperhub_payout_legs l WHERE l.run_id = r.id)
		       AND NOT EXISTS (SELECT 1 FROM keeperhub_payout_legs l
		                       WHERE l.run_id = r.id AND l.status <> 'confirmed')
		      THEN 'complete' ELSE 'dispatching' END,
		    updated_at = now()
		WHERE r.id = $1
		  -- A failed run stays failed. Recomputing its state from the legs would
		  -- let resolving them quietly re-open a run that paid on the wrong
		  -- chain; leaving 'failed' has to be a deliberate act, not a side effect.
		  AND r.state <> 'failed'`, runID)
	return err
}

func (s *Service) runComplete(ctx context.Context, runID uuid.UUID) (bool, error) {
	var state string
	if err := s.Pool.QueryRow(ctx, `SELECT state FROM keeperhub_payout_runs WHERE id = $1`, runID).Scan(&state); err != nil {
		return false, err
	}
	return state == "complete", nil
}

// ResolveRequest is a person settling an unknown leg against the chain.
type ResolveRequest struct {
	HackathonID uuid.UUID
	LegID       uuid.UUID
	ActorID     uuid.UUID
	// Status is "confirmed" (the transfer landed; TxHash required) or "failed"
	// (nothing landed; the leg becomes eligible to be sent again).
	Status string
	TxHash string
	Note   string
}

// ResolveLeg records a person's finding about an unknown leg.
//
// Only unknown legs. A dispatched leg is resolved by Intake, and a confirmed
// one is final. The note is required in both directions: "failed" is the
// dangerous one - it makes the leg sendable again - so the reason somebody
// believed nothing landed has to be written down next to the decision.
//
// This does not read the chain itself. It records what a person established.
func (s *Service) ResolveLeg(ctx context.Context, req ResolveRequest) error {
	note := strings.TrimSpace(req.Note)
	if req.ActorID == uuid.Nil || note == "" {
		return ErrResolutionIncomplete
	}
	if req.Status != string(keeperhub.LegConfirmed) && req.Status != string(keeperhub.LegFailed) {
		return fmt.Errorf("%w: status must be confirmed or failed", ErrResolutionIncomplete)
	}
	if req.Status == string(keeperhub.LegConfirmed) && strings.TrimSpace(req.TxHash) == "" {
		return fmt.Errorf("%w: a confirmation needs the transaction that proves it", ErrResolutionIncomplete)
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var runID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `
		SELECT l.run_id, l.status FROM keeperhub_payout_legs l
		JOIN keeperhub_payout_runs r ON r.id = l.run_id
		WHERE l.id = $1 AND r.hackathon_id = $2 FOR UPDATE OF l`, req.LegID, req.HackathonID).Scan(&runID, &status)
	if isNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != string(keeperhub.LegUnknown) {
		return fmt.Errorf("%w: leg %s is %q", ErrLegNotResolvable, req.LegID, status)
	}

	var txHash any
	if req.Status == string(keeperhub.LegConfirmed) {
		txHash = strings.TrimSpace(req.TxHash)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE keeperhub_payout_legs
		SET status = $2, tx_hash = COALESCE($3, tx_hash), resolved_by = $4, resolution_note = $5,
		    confirmed_at = CASE WHEN $2 = 'confirmed' THEN now() ELSE confirmed_at END,
		    updated_at = now()
		WHERE id = $1`, req.LegID, req.Status, txHash, req.ActorID, note); err != nil {
		return err
	}
	if err := setRunState(ctx, tx, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
