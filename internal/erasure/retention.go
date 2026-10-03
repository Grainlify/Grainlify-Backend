package erasure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// querier is what the selections below need: the pool, or a transaction. The
// pass and its dry run (dryrun.go) select through the same functions, so what
// the dry run reports is what the pass would act on.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (r *Retention) resetCutoff(now time.Time) time.Time { return now.Add(-ResetRecordRetention) }

func (r *Retention) payoutCutoff(now time.Time) time.Time {
	return now.AddDate(-PayoutRecordYears, 0, 0)
}

// resetBatch is how many reset records one pass takes on.
const resetBatch = 500

// resetRecordsDue lists the reset records past ResetRecordRetention ($1 is the
// cutoff), oldest first, at most resetBatch of them. stillUsed is whether the
// session is still somebody's current one or named by a newer reset.
const resetRecordsDue = `
SELECT a.id, a.created_at, COALESCE(a.reason_code, ''), COALESCE(a.previous_session_id, ''),
       COALESCE(a.previous_session_id, '') <> '' AND (
         EXISTS (SELECT 1 FROM users u WHERE u.kyc_session_id = a.previous_session_id)
         OR EXISTS (SELECT 1 FROM kyc_reset_audit o
                    WHERE o.previous_session_id = a.previous_session_id AND o.id <> a.id AND o.created_at >= $1))
FROM kyc_reset_audit a
WHERE a.created_at < $1
ORDER BY a.created_at, a.id
LIMIT 500`

// dueReset is one reset record past its period.
type dueReset struct {
	id         uuid.UUID
	at         time.Time
	reasonCode string
	session    string
	stillUsed  bool
}

// needsDidit is whether erasing the record first needs Didit to delete its
// session.
func (d dueReset) needsDidit() bool { return d.session != "" && !d.stillUsed }

func selectResetRecords(ctx context.Context, q querier, cutoff time.Time) ([]dueReset, error) {
	rows, err := q.Query(ctx, resetRecordsDue, cutoff)
	if err != nil {
		return nil, fmt.Errorf("retention: list reset records: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueReset, error) {
		var d dueReset
		err := row.Scan(&d.id, &d.at, &d.reasonCode, &d.session, &d.stillUsed)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("retention: list reset records: %w", err)
	}
	return list, nil
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
	cutoff := r.resetCutoff(r.now())
	list, err := selectResetRecords(ctx, r.pool, cutoff)
	if err != nil {
		return 0, err
	}

	var erase []uuid.UUID
	var firstErr error
	for _, d := range list {
		if d.needsDidit() {
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
// PayoutRecordYears ago ($1 is the cutoff), with the date that counts from
// (paid_at): the last payment on any rail, or the end of the event if it
// paid nothing, and nothing of it still open. An event with a payout still
// unreleased or unconfirmed is never in it.
//
// The Solana rail (internal/grainhack): a payment is the agent's report of it
// (grainhack_payment_reports.reported_at), and a line still payable in the
// latest statement with no payment reported is open. A line held for KYC is
// not: like a KeeperHub exclusion, it waits for the person, who may never
// verify, and must not keep everyone else's records for ever.
const paidEvents = `
SELECT e.id, e.paid_at FROM (
  SELECT h.id, COALESCE(
           GREATEST(
             (SELECT max(s.released_at) FROM settlements s WHERE s.hackathon_id = h.id),
             (SELECT max(l.confirmed_at) FROM keeperhub_payout_legs l
                JOIN keeperhub_payout_runs r ON r.id = l.run_id WHERE r.hackathon_id = h.id),
             (SELECT max(g.reported_at) FROM grainhack_payment_reports g WHERE g.hackathon_id = h.id)),
           h.ends_at) AS paid_at
  FROM hackathons h
  WHERE NOT EXISTS (SELECT 1 FROM settlements s WHERE s.hackathon_id = h.id AND s.released_at IS NULL)
    AND NOT EXISTS (SELECT 1 FROM keeperhub_payout_legs l JOIN keeperhub_payout_runs r ON r.id = l.run_id
                    WHERE r.hackathon_id = h.id AND l.status IN ('pending', 'dispatched', 'unknown'))
    AND NOT EXISTS (SELECT 1 FROM grainhack_results_statements gs
                    JOIN grainhack_results_statement_lines gl ON gl.statement_id = gs.id
                    WHERE gs.hackathon_id = h.id AND gl.status = 'payable'
                      AND NOT EXISTS (SELECT 1 FROM grainhack_results_statements n WHERE n.supersedes = gs.id)
                      AND NOT EXISTS (SELECT 1 FROM grainhack_payment_reports g
                                      WHERE g.hackathon_id = gs.hackathon_id AND g.pool = gs.pool
                                        AND g.github_user_id = gl.github_user_id))) e
WHERE e.paid_at < $1`

// retentionStep is one table of the payout retention: which rows are due, and
// what is done to them.
type retentionStep struct {
	// name is the table, and the key in RunOnce's counts.
	name string
	// sel selects the rows due, as (id, the date their period counts from),
	// with $1 = the cutoff. The pass and the dry run both select through it.
	sel string
	// act is what the pass does to the selected rows, with $1 = their ids.
	// Empty means delete them.
	act string
}

func (s retentionStep) action() string {
	if s.act == "" {
		return "DELETE FROM " + s.name + " WHERE id = ANY($1)"
	}
	return s.act
}

// verb is what the pass does to a selected row, in the dry run's words.
func (s retentionStep) verb() string {
	if s.act == "" {
		return "delete"
	}
	return "update"
}

// payoutRetentionSteps erase an erased account's payout records once the
// payment is PayoutRecordYears old.
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
//
// Every step's rows are selected before any is acted on (purgePayoutRecords),
// so no step's selection depends on what an earlier one deleted. A row a step
// deletes must not also be taken by the cascade of an earlier step, or the
// pass would erase fewer rows than it selected and refuse to commit; hence
// appeals before their verdicts.
var payoutRetentionSteps = []retentionStep{
	{name: "sponsored_claims", sel: `
SELECT c.id, c.created_at FROM sponsored_claims c
WHERE c.user_id IN (` + erasedAccounts + `) AND c.created_at < $1`},
	// A failed leg nobody resolved may still be owed; it stays.
	{name: "keeperhub_payout_legs", sel: `
SELECT l.id, COALESCE(l.confirmed_at, l.updated_at) FROM keeperhub_payout_legs l
WHERE l.user_id IN (` + erasedAccounts + `)
  AND (l.status = 'confirmed' OR (l.status = 'failed' AND l.resolution_note IS NOT NULL))
  AND COALESCE(l.confirmed_at, l.updated_at) < $1`},
	{name: "keeperhub_payout_exclusions", sel: `
SELECT x.id, x.created_at FROM keeperhub_payout_exclusions x
WHERE x.user_id IN (` + erasedAccounts + `) AND x.created_at < $1`},
	{name: "settlement_holds", sel: `
SELECT h.id, h.released_at FROM settlement_holds h
WHERE h.user_id IN (` + erasedAccounts + `) AND h.released_at < $1`},
	{name: "settlement_lines", sel: `
SELECT l.id, s.released_at FROM settlement_lines l JOIN settlements s ON s.id = l.settlement_id
WHERE l.user_id IN (` + erasedAccounts + `) AND s.released_at < $1`},
	{name: "redemptions", sel: `
SELECT r.id, COALESCE(r.reviewed_at, r.created_at) FROM redemptions r
WHERE r.user_id IN (` + erasedAccounts + `) AND r.status <> 'pending' AND COALESCE(r.reviewed_at, r.created_at) < $1`},
	// The payout belongs to the project; only who received it goes.
	{name: "hackathon_maintainer_payouts", sel: `
SELECT p.id, COALESCE(p.holdback_resolved_at, p.created_at) FROM hackathon_maintainer_payouts p
WHERE p.maintainer_user_id IN (` + erasedAccounts + `)
  AND p.holdback_status <> 'pending' AND COALESCE(p.holdback_resolved_at, p.created_at) < $1`,
		act: `UPDATE hackathon_maintainer_payouts SET maintainer_user_id = NULL, updated_at = now() WHERE id = ANY($1)`},

	// GrainHack: the records that decided the payout, login already replaced
	// at erasure. Model calls first: they carry the pull request the verdict
	// judged, and their link to the verdict is SET NULL, so deleting the
	// verdict alone would orphan them. Appeals before their verdict, which
	// would otherwise take them by cascade; clarity ratings go with their
	// assignment (ON DELETE CASCADE).
	{name: "hackathon_model_calls", sel: `
SELECT m.id, pe.paid_at FROM hackathon_model_calls m
JOIN hackathon_verdicts v ON v.id = m.verdict_id
JOIN (` + paidEvents + `) pe ON pe.id = v.hackathon_id
WHERE v.user_id IN (` + erasedAccounts + `)`},
	{name: "hackathon_appeals", sel: `
SELECT a.id, pe.paid_at FROM hackathon_appeals a
JOIN (` + paidEvents + `) pe ON pe.id = a.hackathon_id
WHERE a.user_id IN (` + erasedAccounts + `)`},
	{name: "hackathon_verdicts", sel: `
SELECT v.id, pe.paid_at FROM hackathon_verdicts v
JOIN (` + paidEvents + `) pe ON pe.id = v.hackathon_id
WHERE v.user_id IN (` + erasedAccounts + `)`},
	{name: "hackathon_assignments", sel: `
SELECT a.id, pe.paid_at FROM hackathon_assignments a
JOIN (` + paidEvents + `) pe ON pe.id = a.hackathon_id
WHERE a.user_id IN (` + erasedAccounts + `)`},
	// A draw is also the record of everybody else who entered it, so it is
	// kept; the person's entry loses its account id (the nil id, which every
	// reader parses), and a draw they won records why it names no winner.
	{name: "hackathon_draws", sel: `
SELECT d.id, pe.paid_at FROM hackathon_draws d
JOIN (` + paidEvents + `) pe ON pe.id = d.hackathon_id
WHERE d.winner_user_id IN (` + erasedAccounts + `)
   OR EXISTS (SELECT 1 FROM jsonb_array_elements(d.pool) e
              WHERE e->>'user_id' IN (SELECT id::text FROM users WHERE erased_at IS NOT NULL))`,
		act: `
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
WHERE d.id = ANY($1)`},

	// GrainHack on Solana (internal/grainhack). The payment the agent
	// reported: amount, transaction and receiving address.
	{name: "grainhack_payment_reports", sel: `
SELECT g.id, pe.paid_at FROM grainhack_payment_reports g
JOIN (` + paidEvents + `) pe ON pe.id = g.hackathon_id
WHERE g.user_id IN (` + erasedAccounts + `)`},
	// The person's lines in every signed results statement of the event. The
	// statement tables refuse DELETE and UPDATE (GH010); the retention
	// transaction is the one place allowed past that, for these rows and
	// these two actions only (statementRetentionSetting).
	{name: "grainhack_results_statement_lines", sel: `
SELECT l.id, pe.paid_at FROM grainhack_results_statement_lines l
JOIN grainhack_results_statements s ON s.id = l.statement_id
JOIN (` + paidEvents + `) pe ON pe.id = s.hackathon_id
WHERE l.user_id IN (` + erasedAccounts + `)`},
	// The signed document still names whoever those lines named, so it is
	// redacted: their lines are taken out of it and the signature, which was
	// over the original bytes, is emptied. Every other winner's line stays,
	// and so do the event pool, total, network and computation in the
	// statement's own columns. After the lines step, so the lines left in the
	// table are the ones the document keeps.
	{name: "grainhack_results_statements", sel: `
SELECT s.id, pe.paid_at FROM grainhack_results_statements s
JOIN (` + paidEvents + `) pe ON pe.id = s.hackathon_id
WHERE EXISTS (SELECT 1 FROM grainhack_results_statement_lines l
              WHERE l.statement_id = s.id AND l.user_id IN (` + erasedAccounts + `))`,
		act: `
UPDATE grainhack_results_statements s
SET canonical_json = jsonb_build_object(
      'redacted', 'lines of erased accounts removed ` + PayoutRecordRedactionNote + `',
      'statement', (COALESCE(s.canonical_json::jsonb -> 'statement', s.canonical_json::jsonb) - 'lines')
        || jsonb_build_object('lines', COALESCE((
             SELECT jsonb_agg(e ORDER BY ord)
             FROM jsonb_array_elements(COALESCE(s.canonical_json::jsonb -> 'statement', s.canonical_json::jsonb) -> 'lines')
                  WITH ORDINALITY AS t(e, ord)
             WHERE EXISTS (SELECT 1 FROM grainhack_results_statement_lines l
                           WHERE l.statement_id = s.id AND l.github_user_id = (e ->> 'github_user_id')::bigint)),
           '[]'::jsonb)))::text,
    signature = '',
    redacted_at = now()
WHERE s.id = ANY($1)`},
}

// PayoutRecordRedactionNote is what a redacted GrainHack results statement
// says about itself, after "lines of erased accounts removed".
const PayoutRecordRedactionNote = "five years after the payment, as the Terms say; the signature was over the original document and no longer applies"

// statementRetentionSetting is the session-local setting that lets the
// retention transaction past the GrainHack statements' immutability trigger
// (migration 20261003120300). Set to the transaction's own id with
// set_config(..., true): it ends with the transaction, and a value set any
// other way does not match. The trigger still allows only the deletion of an
// erased account's lines and the redaction of a statement, and logs each to
// grainhack_results_retention_log.
const statementRetentionSetting = "grainlify.grainhack_results_retention"

// plannedStep is a payout retention step with the rows it selected.
type plannedStep struct {
	step           retentionStep
	ids            []uuid.UUID
	oldest, newest *time.Time
}

// planPayoutRecords selects every payout retention step's rows, before any is
// acted on.
func planPayoutRecords(ctx context.Context, q querier, cutoff time.Time) ([]plannedStep, error) {
	out := make([]plannedStep, 0, len(payoutRetentionSteps))
	for _, st := range payoutRetentionSteps {
		rows, err := q.Query(ctx, st.sel, cutoff)
		if err != nil {
			return nil, fmt.Errorf("retention: select %s: %w", st.name, err)
		}
		p := plannedStep{step: st}
		for rows.Next() {
			var id uuid.UUID
			var at *time.Time
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return nil, fmt.Errorf("retention: select %s: %w", st.name, err)
			}
			p.ids = append(p.ids, id)
			if at != nil {
				if p.oldest == nil || at.Before(*p.oldest) {
					p.oldest = at
				}
				if p.newest == nil || at.After(*p.newest) {
					p.newest = at
				}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("retention: select %s: %w", st.name, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// purgePayoutRecords runs payoutRetentionSteps in one transaction: a round's
// records go together or not at all.
//
// It selects first (planPayoutRecords, the dry run's selection) and then acts
// on exactly the rows selected. Repeatable read, so every selection sees the
// same snapshot and a row changed by somebody else meanwhile fails the pass
// rather than being acted on from a stale read. If any step would touch a
// different number of rows than it selected, nothing is committed: the dry
// run would then have said something the pass did not do.
func (r *Retention) purgePayoutRecords(ctx context.Context) (map[string]int64, error) {
	cutoff := r.payoutCutoff(r.now())
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("retention: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	plan, err := planPayoutRecords(ctx, tx, cutoff)
	if err != nil {
		return nil, err
	}
	// Only this transaction, and only when it has GrainHack statement rows
	// to act on, may pass the statements' immutability trigger.
	for _, p := range plan {
		if strings.HasPrefix(p.step.name, "grainhack_results_") && len(p.ids) > 0 {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, txid_current()::text, true)`, statementRetentionSetting); err != nil {
				return nil, fmt.Errorf("retention: allow statement retention: %w", err)
			}
			break
		}
	}
	out := make(map[string]int64, len(plan))
	for _, p := range plan {
		out[p.step.name] = 0
		if len(p.ids) == 0 {
			continue
		}
		tag, err := tx.Exec(ctx, p.step.action(), p.ids)
		if err != nil {
			return nil, fmt.Errorf("retention: %s: %w", p.step.name, err)
		}
		if tag.RowsAffected() != int64(len(p.ids)) {
			return nil, fmt.Errorf("retention: %s: selected %d rows but %s %d; nothing committed",
				p.step.name, len(p.ids), p.step.verb()+"d", tag.RowsAffected())
		}
		out[p.step.name] = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("retention: commit: %w", err)
	}
	return out, nil
}
