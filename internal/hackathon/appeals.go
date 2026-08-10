package hackathon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AI-specs.md §6.
//
//	- Opens at Phase 5, lasts appeal_window_days.
//	- Contributor sees their full verdict record: criteria, citations, bucket,
//	  reasoning.
//	- Appeal routes to a human with both model verdicts and the diff.
//	- Human decision is final and recorded.
//	- Payouts release at Phase 6, after the appeal window closes.

// Errors callers distinguish rather than string-match.
var (
	ErrAppealWindowClosed = errors.New("the appeal window is not open")
	ErrAppealNotYours     = errors.New("that verdict belongs to someone else")
	ErrAppealExists       = errors.New("this verdict has already been appealed")
	ErrAppealDecided      = errors.New("this appeal already has a decision, and a decision is final")
)

// AppealWindow describes when a hackathon's appeal window opens and closes.
type AppealWindow struct {
	// Days is the configured appeal_window_days at the time of reading.
	Days int `json:"days"`
	// OpensAt is nil until results are published; the window is anchored to
	// that moment rather than to a planned date, because §1 makes phase
	// transitions an explicit admin action that may not happen on schedule.
	OpensAt  *time.Time `json:"opens_at"`
	ClosesAt *time.Time `json:"closes_at"`
	Open     bool       `json:"open"`
	// ClosedOutAt is set once the window has been formally closed and the
	// §13-#4 recompute has run.
	ClosedOutAt *time.Time `json:"closed_out_at"`
}

// GetAppealWindow computes the window for an already-loaded hackathon.
func GetAppealWindow(ctx context.Context, pool db.DBPool, h *Hackathon) (AppealWindow, error) {
	w := AppealWindow{ClosedOutAt: h.AppealsClosedAt}

	raw, err := EffectiveValue(ctx, pool, &h.ID, "appeal_window_days")
	if err != nil {
		return w, fmt.Errorf("hackathon.GetAppealWindow: read appeal_window_days: %w", err)
	}
	w.Days = atoiOr(raw, 7)
	if w.Days < 0 {
		w.Days = 0
	}

	if h.ResultsPublishedAt == nil {
		return w, nil
	}
	opens := *h.ResultsPublishedAt
	closes := opens.AddDate(0, 0, w.Days)
	w.OpensAt = &opens
	w.ClosesAt = &closes

	// A window that has already been closed out stays closed even if someone
	// later lengthens appeal_window_days: the recompute has run and the
	// numbers are settled.
	w.Open = h.AppealsClosedAt == nil && time.Now().Before(closes)
	return w, nil
}

// GetAppealWindowByID is the convenience form for callers holding only an id.
func GetAppealWindowByID(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (AppealWindow, error) {
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return AppealWindow{}, err
	}
	return GetAppealWindow(ctx, pool, h)
}

// SubmitAppealInput is one contributor contesting one verdict.
type SubmitAppealInput struct {
	VerdictID uuid.UUID
	UserID    uuid.UUID
	Reason    string
}

// SubmitAppeal records an appeal against a verdict.
//
// Every gate here is about the appeal being answerable: the window has to be
// open, the verdict has to be the caller's own, and there has to be a stated
// reason for a human to respond to.
func SubmitAppeal(ctx context.Context, pool db.DBPool, in SubmitAppealInput) (uuid.UUID, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return uuid.Nil, fmt.Errorf("hackathon.SubmitAppeal: an appeal needs a reason - a reviewer has nothing to act on otherwise")
	}

	var (
		hackathonID uuid.UUID
		verdictUser *uuid.UUID
		login       string
	)
	err := pool.QueryRow(ctx, `
SELECT hackathon_id, user_id, github_login FROM hackathon_verdicts WHERE id = $1
`, in.VerdictID).Scan(&hackathonID, &verdictUser, &login)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("hackathon.SubmitAppeal: verdict %s not found", in.VerdictID)
		}
		return uuid.Nil, fmt.Errorf("hackathon.SubmitAppeal: load verdict: %w", err)
	}
	if verdictUser == nil || *verdictUser != in.UserID {
		return uuid.Nil, ErrAppealNotYours
	}

	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return uuid.Nil, err
	}
	if h.Phase != "results_published" {
		return uuid.Nil, fmt.Errorf("%w: appeals open when results are published and close before payouts release (this hackathon is %q)", ErrAppealWindowClosed, h.Phase)
	}
	window, err := GetAppealWindow(ctx, pool, h)
	if err != nil {
		return uuid.Nil, err
	}
	if !window.Open {
		return uuid.Nil, fmt.Errorf("%w: it closed on %s", ErrAppealWindowClosed, window.ClosesAt.Format(time.RFC3339))
	}

	var id uuid.UUID
	err = pool.QueryRow(ctx, `
INSERT INTO hackathon_appeals (hackathon_id, verdict_id, user_id, github_login, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (verdict_id) DO NOTHING
RETURNING id
`, hackathonID, in.VerdictID, in.UserID, login, reason).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returns no row, which is the unique index on
		// verdict_id doing its job.
		return uuid.Nil, ErrAppealExists
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("hackathon.SubmitAppeal: insert: %w", err)
	}
	return id, nil
}

// AppealDecision is a human's final answer to one appeal.
type AppealDecision struct {
	AppealID   uuid.UUID
	ReviewerID uuid.UUID
	// Upheld means the contributor was right. NewBucket is then the bucket
	// that actually counts; leaving it empty upholds the appeal without
	// changing the bucket, which is legitimate (e.g. a factual correction to
	// the reasoning that does not move the outcome).
	Upheld    bool
	NewBucket string
	Reason    string
}

// DecideAppeal records the human decision, and applies it to the verdict when
// the appeal is upheld with a different bucket.
//
// The reason is mandatory in both directions. A rejected appeal without one
// tells the contributor nothing, and an upheld one without one is exactly the
// data point the §8 calibration set is missing - a case where a human and the
// model disagreed, and why.
func DecideAppeal(ctx context.Context, pool db.DBPool, d AppealDecision) error {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		return fmt.Errorf("hackathon.DecideAppeal: a written reason is required - it is what the contributor is told, and what the calibration set learns from")
	}
	if d.ReviewerID == uuid.Nil {
		return fmt.Errorf("hackathon.DecideAppeal: no reviewer recorded")
	}
	if d.NewBucket != "" && !validBucket(d.NewBucket) {
		return fmt.Errorf("hackathon.DecideAppeal: %q is not a bucket", d.NewBucket)
	}
	if !d.Upheld && d.NewBucket != "" {
		return fmt.Errorf("hackathon.DecideAppeal: a rejected appeal cannot also change the bucket")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("hackathon.DecideAppeal: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		verdictID uuid.UUID
		status    string
	)
	err = tx.QueryRow(ctx, `
SELECT verdict_id, status FROM hackathon_appeals WHERE id = $1 FOR UPDATE
`, d.AppealID).Scan(&verdictID, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("hackathon.DecideAppeal: appeal %s not found", d.AppealID)
		}
		return fmt.Errorf("hackathon.DecideAppeal: load appeal: %w", err)
	}
	if status != "pending" {
		return ErrAppealDecided
	}

	newStatus := "rejected"
	if d.Upheld {
		newStatus = "upheld"
	}
	var bucket *string
	if d.NewBucket != "" {
		b := d.NewBucket
		bucket = &b
	}

	if _, err := tx.Exec(ctx, `
UPDATE hackathon_appeals
SET status = $1, reviewer_id = $2, decision_reason = $3, decided_bucket = $4,
    decided_at = now(), updated_at = now()
WHERE id = $5
`, newStatus, d.ReviewerID, reason, bucket, d.AppealID); err != nil {
		return fmt.Errorf("hackathon.DecideAppeal: update appeal: %w", err)
	}

	// An upheld appeal that moves the bucket goes through the same
	// human_override path an admin override uses, so there is exactly one way
	// a final bucket ever changes and one place to look for why.
	if d.Upheld && bucket != nil {
		if _, err := tx.Exec(ctx, `
UPDATE hackathon_verdicts
SET final_bucket = $1,
    final_source = 'human_override',
    overridden_by = $2,
    override_reason = $3,
    overridden_at = now(),
    updated_at = now()
WHERE id = $4
`, *bucket, d.ReviewerID, "Appeal upheld: "+reason, verdictID); err != nil {
			return fmt.Errorf("hackathon.DecideAppeal: apply bucket to verdict: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("hackathon.DecideAppeal: commit: %w", err)
	}
	return nil
}

func validBucket(b string) bool {
	for _, x := range BucketOrder {
		if x == b {
			return true
		}
	}
	return false
}

// CloseAppealsAndRecompute closes the appeal window and recomputes the whole
// payout once, per AI-specs.md §13-#4.
//
// The recompute is the point. An upheld appeal changes someone's bucket,
// which changes total_units, which changes unit_value for *everyone* - so
// recomputing only the appellant's row would leave the payouts no longer
// summing to the advertised pool. Doing it per appeal would do the same work
// repeatedly and still be wrong until the last one landed, which is why the
// spec says once, at window close.
//
// Idempotent on hackathons.appeals_closed_at: calling it twice does not
// produce a second recompute, because "divide the pool again" is not a safe
// thing to do twice.
//
// This computes a payout run. It does not release money - that still requires
// GuardPayoutRelease and an explicit admin action.
func CloseAppealsAndRecompute(ctx context.Context, pool db.DBPool, hackathonID, actorID uuid.UUID) (*PayoutPlan, error) {
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return nil, err
	}
	if h.AppealsClosedAt != nil {
		return nil, nil // already closed out; nothing to redo
	}

	var pending int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_appeals WHERE hackathon_id = $1 AND status = 'pending'
`, hackathonID).Scan(&pending); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: count pending: %w", err)
	}
	if pending > 0 {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: %d appeal(s) still pending - every appeal gets a human answer before the pool is divided", pending)
	}

	cfg, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: read config: %w", err)
	}

	var poolAmount float64
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(contributor_prize_pool, 0) FROM hackathons WHERE id = $1
`, hackathonID).Scan(&poolAmount); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: read prize pool: %w", err)
	}

	rows, err := pool.Query(ctx, `
SELECT id::text, github_login, final_bucket
FROM hackathon_verdicts
WHERE hackathon_id = $1 AND final_bucket IS NOT NULL
ORDER BY id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: load verdicts: %w", err)
	}
	var judged []JudgedPR
	for rows.Next() {
		var p JudgedPR
		if err := rows.Scan(&p.VerdictID, &p.Login, &p.Bucket); err != nil {
			rows.Close()
			return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: scan verdict: %w", err)
		}
		judged = append(judged, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: iterate verdicts: %w", err)
	}

	plan, err := ComputePayout(judged, poolAmount, cfg)
	if err != nil {
		return nil, err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-check under the transaction so two concurrent closes cannot both
	// decide they are the first.
	var stillOpen bool
	if err := tx.QueryRow(ctx, `
SELECT appeals_closed_at IS NULL FROM hackathons WHERE id = $1 FOR UPDATE
`, hackathonID).Scan(&stillOpen); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: lock hackathon: %w", err)
	}
	if !stillOpen {
		return nil, nil
	}

	var priorRun *uuid.UUID
	var prior uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT id FROM hackathon_payout_runs WHERE hackathon_id = $1 ORDER BY created_at DESC LIMIT 1
`, hackathonID).Scan(&prior)
	switch {
	case err == nil:
		priorRun = &prior
	case errors.Is(err, pgx.ErrNoRows):
		// No earlier run: this recompute is also the first computation.
	default:
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: find prior run: %w", err)
	}

	var runID uuid.UUID
	if err := tx.QueryRow(ctx, `
INSERT INTO hackathon_payout_runs (
  hackathon_id, contributor_prize_pool, total_units, unit_value,
  floor_applied, payout_floor, unfunded_count, computed_by, trigger, supersedes_run_id
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'appeal_recompute',$9)
RETURNING id
`, hackathonID, plan.Pool, plan.TotalUnits, plan.UnitValue,
		plan.FloorApplied, plan.PayoutFloor, plan.UnfundedCount, actorID, priorRun).Scan(&runID); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: insert payout run: %w", err)
	}

	for _, e := range plan.Entries {
		amount := e.Amount
		if !e.Funded {
			amount = 0
		}
		if _, err := tx.Exec(ctx, `
UPDATE hackathon_verdicts
SET units = $1, payout_amount = $2, payout_run_id = $3, updated_at = now()
WHERE id = $4
`, e.Units, amount, runID, e.VerdictID); err != nil {
			return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: write verdict payout: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE hackathons SET appeals_closed_at = now(), updated_at = now() WHERE id = $1
`, hackathonID); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: stamp appeals_closed_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("hackathon.CloseAppealsAndRecompute: commit: %w", err)
	}
	return plan, nil
}
