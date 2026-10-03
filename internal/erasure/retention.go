package erasure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

// Retention erases what is kept only for a fixed time, once that time is up.
//
// The Terms give each kept category a period ("kept for 90 days, then
// erased"). A period with nothing acting on it is a promise nobody keeps, so
// each one is a pass here, run daily, rather than a sentence alone.
//
// Each pass only removes rows already past their own period, and is safe to
// run again: a second run the same day finds nothing.
type Retention struct {
	pool     db.DBPool
	ext      External
	interval time.Duration
	now      func() time.Time
}

// ResetRecordRetention is how long the record of an administrator's
// identity-verification reset is kept: long enough to answer "was this
// person reset, by whom, and why" while a fraud question about it is still
// likely; then it is erased, for everybody, not only for erased accounts.
const ResetRecordRetention = 90 * 24 * time.Hour

// PayoutRecordYears is how long the payout records of an erased account are
// kept after the payment: amount, date, transaction id, receiving address,
// and the GrainHack records that decided it. Long enough for tax and
// anti-money-laundering record keeping and for disputes; then they are erased
// (payoutRetentionSteps).
const PayoutRecordYears = 5

// NewRetention builds the daily retention pass. ext is the same External the
// executor uses: a reset record names a Didit session, and erasing the record
// asks Didit to delete that session too.
func NewRetention(pool db.DBPool, ext External, interval time.Duration) *Retention {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Retention{pool: pool, ext: ext, interval: interval, now: time.Now}
}

// Run passes on the interval until the context ends. It passes once
// immediately, so a deploy does not postpone what is already due.
func (r *Retention) Run(ctx context.Context) {
	pass := func() {
		n, err := r.RunOnce(ctx)
		if err != nil {
			slog.Error("retention pass failed", "error", err)
		}
		var total int64
		for _, v := range n {
			total += v
		}
		if total > 0 {
			slog.Info("retention pass", "erased", n)
		}
	}
	pass()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

// RunOnce runs every retention step once and reports what each erased. A step
// that fails does not stop the others.
func (r *Retention) RunOnce(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	var firstErr error
	n, err := r.purgeResetRecords(ctx)
	out["kyc_reset_audit"] = n
	if err != nil {
		firstErr = err
	}
	payout, err := r.purgePayoutRecords(ctx)
	for k, v := range payout {
		out[k] = v
	}
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return out, firstErr
}

// purgeResetRecords deletes kyc_reset_audit rows older than
// ResetRecordRetention.
//
// A reset record names the Didit session that was detached, and that session
// still holds the person's document data and images at Didit, which keeps it
// with no time limit unless we delete it. Deleting our record alone would
// orphan it there: nothing else names it, so not even erasing the account
// later could find it (Executor.diditSessions reads it from these rows). So
// the session is deleted at Didit first, and the row only once Didit has
// confirmed it, or answered that it is already gone.
//
// A session still in use - the person's current one, or named by a newer
// reset - is left at Didit; only the old row goes.
//
// When Didit cannot be reached, or is not configured, the row stays and the
// next pass tries again: a record kept a day late is a smaller harm than a
// session nobody can find again.
func (r *Retention) purgeResetRecords(ctx context.Context) (int64, error) {
	cutoff := r.now().Add(-ResetRecordRetention)
	rows, err := r.pool.Query(ctx, `
SELECT a.id, COALESCE(a.previous_session_id, ''),
       COALESCE(a.previous_session_id, '') <> '' AND (
         EXISTS (SELECT 1 FROM users u WHERE u.kyc_session_id = a.previous_session_id)
         OR EXISTS (SELECT 1 FROM kyc_reset_audit o
                    WHERE o.previous_session_id = a.previous_session_id AND o.id <> a.id AND o.created_at >= $1))
FROM kyc_reset_audit a
WHERE a.created_at < $1
ORDER BY a.created_at
LIMIT 500
`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention: list reset records: %w", err)
	}
	type due struct {
		id        uuid.UUID
		session   string
		stillUsed bool
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		err := row.Scan(&d.id, &d.session, &d.stillUsed)
		return d, err
	})
	if err != nil {
		return 0, fmt.Errorf("retention: list reset records: %w", err)
	}

	var erase []uuid.UUID
	var firstErr error
	for _, d := range list {
		if d.session != "" && !d.stillUsed {
			err := r.ext.DeleteDiditSession(ctx, d.session)
			switch {
			case err == nil, errors.Is(err, didit.ErrSessionNotFound):
			case errors.Is(err, ErrNotConfigured):
				slog.Warn("retention: reset record kept: Didit is not configured, so its session cannot be deleted", "audit_id", d.id)
				continue
			default:
				// Only the audit id: the session id is the key to the
				// person's verification at Didit.
				slog.Warn("retention: reset record kept: Didit session delete failed; retrying next pass", "audit_id", d.id, "error", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("retention: didit delete for reset record %s: %w", d.id, err)
				}
				continue
			}
		}
		erase = append(erase, d.id)
	}
	if len(erase) == 0 {
		return 0, firstErr
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM kyc_reset_audit WHERE id = ANY($1) AND created_at < $2`, erase, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention: delete reset records: %w", err)
	}
	return tag.RowsAffected(), firstErr
}

// erasedAccounts is every tombstone. Each payout retention step is limited to
// it: an account that still exists keeps its payout history for as long as it
// exists, as the Terms say.
const erasedAccounts = `SELECT id FROM users WHERE erased_at IS NOT NULL`

// paidEvents is every GrainHack event whose payouts were all made more than
// PayoutRecordYears ago ($1 is the cutoff): the last payment on either rail,
// or the end of the event if it paid nothing, and nothing of it still open.
// An event with a payout still unreleased or unconfirmed is never in it.
const paidEvents = `
SELECT h.id FROM hackathons h
WHERE COALESCE(
        GREATEST(
          (SELECT max(s.released_at) FROM settlements s WHERE s.hackathon_id = h.id),
          (SELECT max(l.confirmed_at) FROM keeperhub_payout_legs l
             JOIN keeperhub_payout_runs r ON r.id = l.run_id WHERE r.hackathon_id = h.id)),
        h.ends_at) < $1
  AND NOT EXISTS (SELECT 1 FROM settlements s WHERE s.hackathon_id = h.id AND s.released_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM keeperhub_payout_legs l JOIN keeperhub_payout_runs r ON r.id = l.run_id
                  WHERE r.hackathon_id = h.id AND l.status IN ('pending', 'dispatched', 'unknown'))`

// payoutRetentionSteps erase an erased account's payout records once the
// payment is PayoutRecordYears old. Each runs with $1 = the cutoff.
//
// # What "erased" means for a payout record
//
// The transfer itself is on a public blockchain and stays there; nobody can
// remove it. What we can remove is everything in our database that connects
// it to the person: the receiving address, the transaction id (which leads to
// the address on chain), the per-person line and its amount, and the
// tombstone's id on each of them. So the rows are deleted, not blanked.
//
// What stays is the total of each payout round - settlements.pool_minor,
// keeperhub_payout_runs.pool_minor, payout_event_roots.total_minor - which
// names nobody, so the books for a round still add up to what left the
// treasury. Deleting rather than blanking also keeps every reader safe: the
// columns these rows hold are NOT NULL and checked (an address must look like
// one, a confirmed leg must have a transaction), and a missing row is
// something every query already handles.
//
// claim_leaves is not here. It has no user id by design (payout.ClaimFor);
// after the erasure nothing connects a leaf to the account, and the list is
// the one whose fingerprint was published on chain, so it is kept as it is
// (policy.go, Retained).
//
// Only what has finished is erased: a leg still pending, a hold not released,
// a settlement not released, a redemption not decided, or an event with any
// of those, stays until it has finished and then for PayoutRecordYears.
var payoutRetentionSteps = []step{
	{"sponsored_claims", `
DELETE FROM sponsored_claims WHERE user_id IN (` + erasedAccounts + `) AND created_at < $1`},
	// A failed leg nobody resolved may still be owed; it stays.
	{"keeperhub_payout_legs", `
DELETE FROM keeperhub_payout_legs
WHERE user_id IN (` + erasedAccounts + `)
  AND (status = 'confirmed' OR (status = 'failed' AND resolution_note IS NOT NULL))
  AND COALESCE(confirmed_at, updated_at) < $1`},
	{"keeperhub_payout_exclusions", `
DELETE FROM keeperhub_payout_exclusions WHERE user_id IN (` + erasedAccounts + `) AND created_at < $1`},
	{"settlement_holds", `
DELETE FROM settlement_holds WHERE user_id IN (` + erasedAccounts + `) AND released_at < $1`},
	{"settlement_lines", `
DELETE FROM settlement_lines l USING settlements s
WHERE s.id = l.settlement_id AND l.user_id IN (` + erasedAccounts + `) AND s.released_at < $1`},
	{"redemptions", `
DELETE FROM redemptions
WHERE user_id IN (` + erasedAccounts + `) AND status <> 'pending' AND COALESCE(reviewed_at, created_at) < $1`},
	// The payout belongs to the project; only who received it goes.
	{"hackathon_maintainer_payouts", `
UPDATE hackathon_maintainer_payouts SET maintainer_user_id = NULL, updated_at = now()
WHERE maintainer_user_id IN (` + erasedAccounts + `)
  AND holdback_status <> 'pending' AND COALESCE(holdback_resolved_at, created_at) < $1`},

	// GrainHack: the records that decided the payout, login already replaced
	// at erasure. Model calls first: they carry the pull request the verdict
	// judged, and their link to the verdict is SET NULL, so deleting the
	// verdict alone would orphan them. Appeals and clarity ratings go with
	// their verdict and assignment (ON DELETE CASCADE).
	{"hackathon_model_calls", `
DELETE FROM hackathon_model_calls
WHERE verdict_id IN (SELECT id FROM hackathon_verdicts
                     WHERE user_id IN (` + erasedAccounts + `) AND hackathon_id IN (` + paidEvents + `))`},
	{"hackathon_verdicts", `
DELETE FROM hackathon_verdicts WHERE user_id IN (` + erasedAccounts + `) AND hackathon_id IN (` + paidEvents + `)`},
	{"hackathon_appeals", `
DELETE FROM hackathon_appeals WHERE user_id IN (` + erasedAccounts + `) AND hackathon_id IN (` + paidEvents + `)`},
	{"hackathon_assignments", `
DELETE FROM hackathon_assignments WHERE user_id IN (` + erasedAccounts + `) AND hackathon_id IN (` + paidEvents + `)`},
	// A draw is also the record of everybody else who entered it, so it is
	// kept; the person's entry loses its account id (the nil id, which every
	// reader parses), and a draw they won records why it names no winner.
	{"hackathon_draws", `
UPDATE hackathon_draws d
SET pool = COALESCE((
      SELECT jsonb_agg(CASE WHEN e->>'user_id' IN (SELECT id::text FROM users WHERE erased_at IS NOT NULL)
                            THEN jsonb_set(e, '{user_id}', '"00000000-0000-0000-0000-000000000000"')
                            ELSE e END ORDER BY ord)
      FROM jsonb_array_elements(d.pool) WITH ORDINALITY AS t(e, ord)), d.pool),
    winner_user_id = CASE WHEN d.winner_user_id IN (` + erasedAccounts + `) THEN NULL ELSE d.winner_user_id END,
    no_winner_reason = CASE WHEN d.winner_user_id IN (` + erasedAccounts + `)
                            THEN 'the winner deleted their account; its records were erased five years after the payout'
                            ELSE d.no_winner_reason END
WHERE d.hackathon_id IN (` + paidEvents + `)
  AND (d.winner_user_id IN (` + erasedAccounts + `)
       OR EXISTS (SELECT 1 FROM jsonb_array_elements(d.pool) e
                  WHERE e->>'user_id' IN (SELECT id::text FROM users WHERE erased_at IS NOT NULL)))`},
}

// purgePayoutRecords runs payoutRetentionSteps in one transaction: a round's
// records go together or not at all.
func (r *Retention) purgePayoutRecords(ctx context.Context) (map[string]int64, error) {
	cutoff := r.now().AddDate(-PayoutRecordYears, 0, 0)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("retention: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	out := make(map[string]int64, len(payoutRetentionSteps))
	for _, st := range payoutRetentionSteps {
		tag, err := tx.Exec(ctx, st.sql, cutoff)
		if err != nil {
			return nil, fmt.Errorf("retention: %s: %w", st.name, err)
		}
		out[st.name] = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("retention: commit: %w", err)
	}
	return out, nil
}
