package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// KYCStatusReconciler re-reads Didit for sessions we already know about and
// writes back a status that changed since we last heard.
//
// # The sibling of KYCReviewSweeper, and its opposite in the one way that matters
//
// That sweep exists for the same reason - Didit retries a failed webhook twice,
// roughly one minute and then four minutes later, and then drops the delivery
// permanently - but it deliberately does NOT change anybody's kyc_status. It
// alerts a human and stops.
//
// That was the right call for the case it covers: a session sitting in review
// is waiting on a decision, and re-attempting produces a duplicate session
// rather than resolving the first. It is the wrong call for a decision that
// has already been made and that we missed. There, the status IS the thing
// that is wrong, and alerting a human to go and fix it by hand is how four
// contributors ended up being UPDATEd against production with no record of who
// did it or why.
//
// So this one writes.
//
// # Why it must re-read sessions that are already verified
//
// The obvious queue is "sessions not yet in a final state". That would miss
// the case this exists for.
//
// A contributor verified with us keeps that status until something changes it,
// and the only thing that can is an inbound event. A reviewer declining an
// already-approved session in Didit's console is precisely the reversal that
// matters, because verification is what admits somebody to a founding wave,
// and nothing downstream re-reads it (see issue #507 - that is a separate
// defect and this does not fix it). If a verified session is never re-read,
// a reversal that arrives as a dropped webhook is invisible forever.
//
// So the queue is every session, ordered by least-recently-checked. Verified
// is not a terminal state; it is a state we should keep confirming.
//
// # It cannot undo an admin reset
//
// Worth stating because it is the first thing a reader will worry about: a
// reset sets kyc_status='expired' while Didit's copy of the session still says
// whatever it said, so a reconciler that re-read that session would flip the
// contributor straight back and silently undo the reset.
//
// It cannot happen here, and not by a check added on this side: KYCAdminHandler's
// reset nulls kyc_session_id in the same statement, so a reset contributor
// leaves this queue entirely. The guard is structural rather than remembered.
// If that reset ever stops nulling the session id, this comment is the thing
// that should stop it.
// sessionDecisionReader is the one thing this needs from Didit.
//
// An interface rather than *didit.Client because didit.BaseURL is a package
// const: the concrete client cannot be pointed at a test server without
// changing a type shared with the webhook and the status poll, and widening
// that type is v3 work with its own review. Narrowing at the consumer costs
// four lines and keeps this change to one subsystem.
type sessionDecisionReader interface {
	GetSessionDecision(ctx context.Context, sessionID string) (didit.SessionDecisionResponse, error)
}

type KYCStatusReconciler struct {
	db     *db.DB
	didit  sessionDecisionReader
	sink   SupportSink
	notify *notifications.Service

	// batch bounds how many sessions are re-read per tick. Each one is a
	// separate Didit API call, so this is the rate limit.
	batch int
	// interval is how often a batch runs.
	interval time.Duration
}

func NewKYCStatusReconciler(cfg config.Config, d *db.DB, notify *notifications.Service) *KYCStatusReconciler {
	r := &KYCStatusReconciler{
		db:     d,
		sink:   newTelegramSupportSink(telegramSinkConfigFrom(cfg)),
		notify: notify,
		// 25 every 5 minutes is 300/hour. Chosen against the size of the
		// population rather than a round number: every session we hold gets
		// re-read within hours, which is the right order for a reversal that
		// currently takes a settlement run to discover, without turning a
		// backstop into a load generator against somebody else's API.
		batch:    25,
		interval: 5 * time.Minute,
	}
	// Assigned only when there is a real client. Assigning a nil
	// *didit.Client to the interface field would produce a non-nil interface
	// wrapping a nil pointer, and Run's "didit == nil" check would pass while
	// every call panicked - the same trap the mailer construction in
	// cmd/api/main.go documents.
	if cfg.DiditAPIKey != "" {
		r.didit = didit.NewClient(cfg.DiditAPIKey)
	}
	return r
}

// Run reconciles until the context is cancelled.
func (r *KYCStatusReconciler) Run(ctx context.Context) {
	if r.db == nil || r.db.Pool == nil {
		slog.Warn("kyc reconciler: no database, not starting")
		return
	}
	if r.didit == nil {
		// Not an error: KYC is optional in local and preview environments.
		// Said out loud anyway, because a reconciler that silently does
		// nothing is indistinguishable from one that finds nothing wrong.
		slog.Warn("kyc reconciler: no Didit client (DIDIT_API_KEY unset), not starting")
		return
	}

	// Once at startup, before the ticker - the same thing KYCReviewSweeper
	// does, for a reason that applies here with more force.
	//
	// A decision that changed while the process was down is exactly what this
	// exists to catch, and waiting a full interval to begin looking is the
	// wrong way round. On a platform that restarts the process on every
	// deploy, it is also worse than it sounds: each deploy pushes the first
	// pass another interval away, so a day of frequent deploys can leave the
	// reconciler having never completed one.
	//
	// This was missed when the file was written as a sibling of the sweeper -
	// the sibling documents the startup pass in a comment, and the comment was
	// read as description rather than as a requirement.
	r.reconcileOnce(ctx)

	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

type reconcileCandidate struct {
	userID    uuid.UUID
	sessionID string
	stored    string
}

// reconcileOnce re-reads one batch. Exported behaviour is per-session and
// independent: one session's failure must not stop the rest of the batch, for
// the same reason a single bad PR must not stop a verdict sync.
func (r *KYCStatusReconciler) reconcileOnce(ctx context.Context) {
	rows, err := r.db.Pool.Query(ctx, `
SELECT id, kyc_session_id, COALESCE(kyc_status, '')
FROM users
WHERE kyc_session_id IS NOT NULL
ORDER BY kyc_reconciled_at NULLS FIRST
LIMIT $1
`, r.batch)
	if err != nil {
		slog.Error("kyc reconciler: queue query failed", "error", err)
		return
	}
	var batch []reconcileCandidate
	for rows.Next() {
		var c reconcileCandidate
		if err := rows.Scan(&c.userID, &c.sessionID, &c.stored); err != nil {
			rows.Close()
			slog.Error("kyc reconciler: scan failed", "error", err)
			return
		}
		batch = append(batch, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("kyc reconciler: queue read failed", "error", err)
		return
	}

	for _, c := range batch {
		r.reconcileOne(ctx, c)
	}
}

func (r *KYCStatusReconciler) reconcileOne(ctx context.Context, c reconcileCandidate) {
	// Stamped FIRST, and whatever happens next.
	//
	// A session that fails every time - the six that answer 403 to our API key
	// - would otherwise stay at the head of a NULLS FIRST queue forever and
	// starve every session behind it. Recording the attempt rather than the
	// success is what keeps the queue rotating. The failure is still visible,
	// in the log, where a repeated one is a signal rather than a stall.
	if _, err := r.db.Pool.Exec(ctx,
		`UPDATE users SET kyc_reconciled_at = now() WHERE id = $1`, c.userID); err != nil {
		slog.Error("kyc reconciler: could not stamp attempt", "user_id", c.userID, "error", err)
		return
	}

	decision, err := r.didit.GetSessionDecision(ctx, c.sessionID)
	if err != nil {
		slog.Warn("kyc reconciler: could not read session, status left unchanged",
			"user_id", c.userID, "session_id", c.sessionID, "stored_status", c.stored, "error", err)
		return
	}

	live, recognised := mapDiditStatus(decision.Status)
	if !recognised {
		// Same rule the webhook follows: an unrecognised status must not
		// overwrite a real one. mapDiditStatus has already logged the value.
		slog.Warn("kyc reconciler: unrecognised status, kyc_status left unchanged",
			"user_id", c.userID, "session_id", c.sessionID, "didit_status", decision.Status)
		return
	}
	if live == c.stored {
		return
	}

	// A verified contributor losing that status is the event this whole
	// reconciler exists for, and it is the one that costs money: verification
	// is what admits somebody to a founding wave. Logged at error level and
	// alerted, because "we discovered this ourselves, hours late" is a
	// materially different thing from a routine status change.
	reversal := c.stored == "verified" && live != "verified"

	decisionJSON, _ := json.Marshal(map[string]interface{}{
		"decision": decision.Decision,
		"data":     decision.Data,
	})

	// kyc_verified_at is stamped only on the TRANSITION into verified, exactly
	// as the webhook does it - the unqualified kyc_status inside the SET is the
	// row's pre-update value. Duplicated deliberately rather than shared: the
	// webhook's comment explains what re-dating this column cost, and a
	// reconciler that observes somebody still verified must not re-date them
	// either.
	if _, err := r.db.Pool.Exec(ctx, `
UPDATE users
SET kyc_status = $1,
    kyc_data = $2,
    kyc_verified_at = CASE
      WHEN $1 = 'verified' AND kyc_status IS DISTINCT FROM 'verified' THEN now()
      ELSE kyc_verified_at
    END,
    updated_at = now()
WHERE id = $3
`, live, decisionJSON, c.userID); err != nil {
		slog.Error("kyc reconciler: status update failed",
			"user_id", c.userID, "session_id", c.sessionID, "error", err)
		return
	}

	if reversal {
		slog.Error("kyc reconciler: VERIFIED STATUS REVERSED, discovered by reconciliation not by webhook",
			"user_id", c.userID, "session_id", c.sessionID, "was", c.stored, "now", live)
		alertAdminOfKYCReversal(ctx, r.db, r.sink, c.userID, c.sessionID, c.stored, live)
	} else {
		slog.Info("kyc reconciler: status corrected",
			"user_id", c.userID, "session_id", c.sessionID, "was", c.stored, "now", live)
	}

	// The same follow-ups the webhook performs, because a decision this
	// reconciler is the first to see is a decision the webhook never
	// delivered. Doing the status write without them would leave the account
	// half-synced: verified with us, but never admitted to a wave and with the
	// referrer never credited - a state no other path can produce and none
	// would repair.
	if live == "verified" && c.stored != "verified" {
		maybeCompleteReferral(ctx, r.db, r.notify, c.userID)
	}
	if live == "in_review" {
		alertAdminOfKYCReview(ctx, r.db, r.sink, c.userID, c.sessionID, "reconciler")
	}
}
