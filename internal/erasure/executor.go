package erasure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/cryptox"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// MaxExternalAttempts is how many passes may fail to reach another service
// before the erasure goes ahead without it. Deferring forever would let one
// unreachable provider block a person's erasure indefinitely; giving up at
// once would skip a step a minute's retry would have done. What could not be
// done is recorded on the request, so it can be finished by hand.
const MaxExternalAttempts = 3

// RetryAfter is the wait between passes that failed to reach a service.
const RetryAfter = time.Hour

// HoldRecheck is how often a held request is looked at again.
const HoldRecheck = 6 * time.Hour

// StepResult is one external step's outcome, as stored on the request.
// Outcome is one of: done, already_gone, not_configured, nothing_to_do,
// failed.
type StepResult struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	// Pending lists identifiers still held at the other service when the step
	// failed for good: Didit session ids, or the GitHub account id the agent
	// knows. Kept so the step can be finished by hand; this is the only
	// identifier left after an erasure, and only when something failed.
	Pending []string `json:"pending,omitempty"`
}

func (r StepResult) failed() bool { return r.Outcome == "failed" }

// Executor carries out due requests.
type Executor struct {
	pool     db.DBPool
	ext      External
	encKey   string
	interval time.Duration
	now      func() time.Time
	// scope, when set, limits a pass to these accounts. Tests only: test
	// packages share one database and run in parallel, and a pass with the
	// clock moved forward would otherwise carry out another package's
	// requests mid-test. Production leaves it nil.
	scope []uuid.UUID
}

// NewExecutor builds an executor. encKeyB64 is TOKEN_ENC_KEY_B64, needed to
// read the GitHub token that RevokeGitHub presents.
func NewExecutor(pool db.DBPool, ext External, encKeyB64 string, interval time.Duration) *Executor {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &Executor{pool: pool, ext: ext, encKey: encKeyB64, interval: interval, now: time.Now}
}

// Run executes due requests on the interval until the context ends. It runs
// once immediately, so a deploy does not postpone a due erasure.
func (e *Executor) Run(ctx context.Context) {
	pass := func() {
		n, err := e.RunOnce(ctx)
		if err != nil {
			slog.Error("account erasure pass failed", "error", err)
		}
		if n > 0 {
			slog.Info("account erasure pass", "requests_examined", n)
		}
	}
	pass()
	t := time.NewTicker(e.interval)
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

// RunOnce examines every due request once and returns how many it examined.
func (e *Executor) RunOnce(ctx context.Context) (int, error) {
	rows, err := e.pool.Query(ctx, `
SELECT id FROM account_deletion_requests
WHERE status IN ('scheduled', 'held')
  AND execute_after <= $1
  AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
  AND ($2::uuid[] IS NULL OR user_id = ANY($2))
ORDER BY execute_after
LIMIT 20
`, e.now(), e.scope)
	if err != nil {
		return 0, fmt.Errorf("erasure: list due: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("erasure: list due: %w", err)
	}
	var firstErr error
	for _, id := range ids {
		if err := e.executeOne(ctx, id); err != nil {
			slog.Error("account erasure failed", "request_id", id, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return len(ids), firstErr
}

// executeOne carries out one request inside one transaction.
//
// The row is locked (SKIP LOCKED, so a second instance passes over it) for
// the whole of the work, including the calls to other services. If anything
// fails before commit the transaction rolls back and the next pass starts
// again from the top - which is safe, because every external step treats
// "already gone" as done.
func (e *Executor) executeOne(ctx context.Context, id uuid.UUID) error {
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit

	var userID *uuid.UUID
	var status string
	var attempts int
	var holdReason *string
	err = tx.QueryRow(ctx, `
SELECT user_id, status, attempts, hold_reason FROM account_deletion_requests
WHERE id = $1 AND status IN ('scheduled', 'held') AND execute_after <= $2
FOR UPDATE SKIP LOCKED
`, id, e.now()).Scan(&userID, &status, &attempts, &holdReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // cancelled, completed, or another instance has it
	}
	if err != nil {
		return err
	}
	if userID == nil {
		// The users row was removed by something other than this path. There
		// is nothing left here to erase; say so rather than spin on it.
		return e.complete(ctx, tx, id, attempts+1, nil, map[string]StepResult{}, "the account row was already gone")
	}
	uid := *userID

	// 1. Money still on its way holds the erasure.
	reasons, err := MoneyInFlight(ctx, tx, uid)
	if err != nil {
		return err
	}
	if len(reasons) > 0 {
		return e.hold(ctx, tx, id, status, holdReason, holdMessage(reasons))
	}

	// 2. What the other services know the person by.
	var ghID *int64
	var ghLogin *string
	var tokenBlob []byte
	err = tx.QueryRow(ctx, `SELECT github_user_id, login, access_token FROM github_accounts WHERE user_id = $1`, uid).
		Scan(&ghID, &ghLogin, &tokenBlob)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	sessions, err := diditSessions(ctx, tx, uid)
	if err != nil {
		return err
	}

	// 3. Other services, the agent first: it is the one that can refuse
	// (an assignment or payout in progress), and a refusal must be found out
	// before anything irreversible has been done anywhere else.
	results := map[string]StepResult{}
	if ghID != nil {
		login := ""
		if ghLogin != nil {
			login = *ghLogin
		}
		err := e.ext.EraseAtAgent(ctx, *ghID, login)
		if errors.Is(err, ErrAgentInFlight) {
			return e.hold(ctx, tx, id, status, holdReason, holdMessage([]string{"a bounty you are assigned to, or a bounty payout, in progress"}))
		}
		results["agent"] = outcome(err, fmt.Sprintf("github:%d", *ghID))
	} else {
		results["agent"] = StepResult{Outcome: "nothing_to_do", Detail: "no GitHub account linked"}
	}

	results["didit"] = e.deleteDidit(ctx, sessions)

	if len(tokenBlob) > 0 {
		token, derr := e.decryptToken(tokenBlob)
		if derr != nil {
			results["github"] = StepResult{Outcome: "failed", Detail: "could not read the stored token: " + derr.Error()}
		} else {
			results["github"] = outcome(e.ext.RevokeGitHub(ctx, token), "")
		}
	} else {
		results["github"] = StepResult{Outcome: "nothing_to_do", Detail: "no GitHub token stored"}
	}

	// 4. A failed step defers the whole erasure, up to the attempt limit.
	attempts++
	var failed []string
	for name, r := range results {
		if r.failed() {
			failed = append(failed, name+": "+r.Detail)
		}
	}
	if len(failed) > 0 && attempts < MaxExternalAttempts {
		return e.postpone(ctx, tx, id, attempts, results, failed)
	}
	// Past the limit, the identifiers needed to finish by hand are kept on
	// the request; on success they are not kept at all.
	for name, r := range results {
		if !r.failed() {
			r.Pending = nil
			results[name] = r
		}
	}

	// 5. Our own database.
	counts, err := eraseRows(ctx, tx, uid)
	if err != nil {
		return err
	}
	return e.complete(ctx, tx, id, attempts, counts, results, "")
}

// outcome turns an external step's error into its stored result. pending is
// the identifier to keep if the step failed.
func outcome(err error, pending string) StepResult {
	switch {
	case err == nil:
		return StepResult{Outcome: "done"}
	case errors.Is(err, ErrNotConfigured):
		return StepResult{Outcome: "not_configured"}
	case errors.Is(err, didit.ErrSessionNotFound), errors.Is(err, github.ErrGrantNotFound):
		return StepResult{Outcome: "already_gone"}
	default:
		r := StepResult{Outcome: "failed", Detail: err.Error()}
		if pending != "" {
			r.Pending = []string{pending}
		}
		return r
	}
}

func (e *Executor) deleteDidit(ctx context.Context, sessions []string) StepResult {
	if len(sessions) == 0 {
		return StepResult{Outcome: "nothing_to_do", Detail: "no verification sessions"}
	}
	var pending []string
	var lastErr error
	notConfigured := false
	for _, s := range sessions {
		err := e.ext.DeleteDiditSession(ctx, s)
		switch {
		case err == nil, errors.Is(err, didit.ErrSessionNotFound):
		case errors.Is(err, ErrNotConfigured):
			notConfigured = true
		default:
			pending = append(pending, s)
			lastErr = err
		}
	}
	if notConfigured {
		// Production always has a key; without one there is no Didit to ask,
		// and the person is told to contact Didit themselves (Terms).
		return StepResult{Outcome: "not_configured", Pending: sessions}
	}
	if len(pending) > 0 {
		return StepResult{Outcome: "failed", Detail: lastErr.Error(), Pending: pending}
	}
	return StepResult{Outcome: "done", Detail: fmt.Sprintf("%d session(s)", len(sessions))}
}

// diditSessions is every Didit session id we hold for the person: the
// current one, any recorded by an administrator reset, and any an alert was
// raised about.
func diditSessions(ctx context.Context, pool Pool, uid uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx, `
SELECT kyc_session_id FROM users WHERE id = $1 AND kyc_session_id IS NOT NULL AND kyc_session_id <> ''
UNION
SELECT previous_session_id FROM kyc_reset_audit WHERE subject_user_id = $1 AND previous_session_id IS NOT NULL AND previous_session_id <> ''
UNION
SELECT session_id FROM kyc_review_alerts WHERE user_id = $1
ORDER BY 1
`, uid)
	if err != nil {
		return nil, fmt.Errorf("erasure: list didit sessions: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (e *Executor) decryptToken(blob []byte) (string, error) {
	key, err := cryptox.KeyFromB64(e.encKey)
	if err != nil {
		return "", err
	}
	b, err := cryptox.DecryptAESGCM(key, blob)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (e *Executor) hold(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string, prev *string, reason string) error {
	if _, err := tx.Exec(ctx, `
UPDATE account_deletion_requests
SET status = 'held', hold_reason = $2, next_attempt_at = $3
WHERE id = $1
`, id, reason, e.now().Add(HoldRecheck)); err != nil {
		return err
	}
	// One event per change of reason, not one per pass: a hold re-checked
	// four times a day for a month is one fact, not a hundred and twenty.
	if status != "held" || prev == nil || *prev != reason {
		if err := recordEvent(ctx, tx, id, "held", map[string]any{"reason": reason}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (e *Executor) postpone(ctx context.Context, tx pgx.Tx, id uuid.UUID, attempts int, results map[string]StepResult, failed []string) error {
	ext, _ := json.Marshal(results)
	msg := fmt.Sprintf("%v", failed)
	if _, err := tx.Exec(ctx, `
UPDATE account_deletion_requests
SET status = 'scheduled', hold_reason = NULL, attempts = $2, next_attempt_at = $3,
    last_error = $4, external = $5
WHERE id = $1
`, id, attempts, e.now().Add(RetryAfter), msg, ext); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, id, "deferred", map[string]any{"attempt": attempts, "failed": failed}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *Executor) complete(ctx context.Context, tx pgx.Tx, id uuid.UUID, attempts int, counts map[string]int64, results map[string]StepResult, note string) error {
	retained := append([]Item(nil), Retained...)
	// Anything an external step could not do is kept on the record, and said
	// so in the list the person sees.
	for name, r := range results {
		if r.failed() || (r.Outcome == "not_configured" && len(r.Pending) > 0) {
			retained = append(retained, Item{
				What:  fmt.Sprintf("Identifiers needed to finish the %s step by hand", name),
				Why:   "The " + name + " step could not be completed automatically (" + r.Outcome + "). They are kept only until an administrator finishes it",
				Until: "until the step is finished by hand",
			})
		}
	}
	erasedJSON, _ := json.Marshal(counts)
	retainedJSON, _ := json.Marshal(retained)
	extJSON, _ := json.Marshal(results)
	if _, err := tx.Exec(ctx, `
UPDATE account_deletion_requests
SET status = 'completed', completed_at = $2, hold_reason = NULL, next_attempt_at = NULL,
    last_error = NULLIF($3, ''), erased = $4, retained = $5, external = $6,
    attempts = $7
WHERE id = $1
`, id, e.now(), note, erasedJSON, retainedJSON, extJSON, attempts); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, id, "completed", map[string]any{"external": results, "note": note}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
