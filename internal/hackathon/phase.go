package hackathon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// PhaseOrder is the sequence Transition enforces - a hackathon may only move
// forward one step at a time, never skip ahead or move backward.
var PhaseOrder = []string{
	"draft",
	"application_period",
	"issue_prep",
	"live",
	"closed",
	// AI-specs.md §1 Phase 5: buckets published, appeal window opens.
	"results_published",
	// Phase 6: contributor payouts release, maintainer holdback timer starts.
	"settled",
}

func phaseIndex(phase string) int {
	for i, p := range PhaseOrder {
		if p == phase {
			return i
		}
	}
	return -1
}

// Hackathon is the subset of the hackathons row Readiness/Transition need.
type Hackathon struct {
	ID                     uuid.UUID
	Phase                  string
	AnnouncedAt            *time.Time
	ApplicationPeriodStart *time.Time
	ApplicationPeriodEnd   *time.Time
	IssuePrepStart         *time.Time
	StartsAt               *time.Time
	EndsAt                 *time.Time
	ResultsPublishedAt     *time.Time
	AppealsClosedAt        *time.Time
}

func loadHackathon(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (*Hackathon, error) {
	var h Hackathon
	err := pool.QueryRow(ctx, `
SELECT id, phase, announced_at, application_period_start, application_period_end, issue_prep_start, starts_at, ends_at,
       results_published_at, appeals_closed_at
FROM hackathons WHERE id = $1
`, hackathonID).Scan(&h.ID, &h.Phase, &h.AnnouncedAt, &h.ApplicationPeriodStart, &h.ApplicationPeriodEnd, &h.IssuePrepStart, &h.StartsAt, &h.EndsAt,
		&h.ResultsPublishedAt, &h.AppealsClosedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("hackathon.loadHackathon: hackathon %s not found", hackathonID)
		}
		return nil, fmt.Errorf("hackathon.loadHackathon: %w", err)
	}
	return &h, nil
}

// BlockingReason describes one requirement not yet met for the next phase
// transition. The admin dashboard shows these directly (AI-specs.md §1:
// "The admin dashboard shows the current phase and what is blocking the
// next one").
type BlockingReason struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// requiredForTransition returns the blocking reasons for moving h into
// toPhase - only ever the phase immediately after h.Phase in PhaseOrder;
// callers must not pass anything else (Transition enforces this).
func requiredForTransition(h *Hackathon, toPhase string) []BlockingReason {
	var reasons []BlockingReason
	switch toPhase {
	case "application_period":
		if h.AnnouncedAt == nil {
			reasons = append(reasons, BlockingReason{"announced_at", "Set an announcement date before opening applications."})
		}
		if h.ApplicationPeriodStart == nil {
			reasons = append(reasons, BlockingReason{"application_period_start", "Set when the application period opens."})
		}
		if h.ApplicationPeriodEnd == nil {
			reasons = append(reasons, BlockingReason{"application_period_end", "Set when the application period closes."})
		}
	case "issue_prep":
		if h.IssuePrepStart == nil {
			reasons = append(reasons, BlockingReason{"issue_prep_start", "Set when issue preparation begins."})
		}
	case "live":
		if h.StartsAt == nil {
			reasons = append(reasons, BlockingReason{"starts_at", "Set when the hackathon goes live."})
		}
		if h.EndsAt == nil {
			reasons = append(reasons, BlockingReason{"ends_at", "Set when the hackathon ends."})
		}
	case "closed":
		// Deliberately unblocked. Closing is how an admin stops the event -
		// requiring in-flight assignments to be resolved first would make
		// the phase that RELEASES them impossible to reach. CloseEvent
		// handles the outstanding work on the way through.
	case "settled":
		// The appeal window is anchored to when results were actually
		// published, which is a stored fact rather than a planned date.
		if h.ResultsPublishedAt == nil {
			reasons = append(reasons, BlockingReason{"results_published_at", "Results have not been published, so no appeal window has opened."})
		}
	}
	return reasons
}

// dynamicBlockers are the requirements that need to look at other tables -
// whether judging finished, whether appeals are still open - rather than at
// the hackathon row alone.
//
// Kept separate from requiredForTransition so that one stays a pure function
// over a loaded row, which is what makes the date/field rules trivially
// testable without a database.
func dynamicBlockers(ctx context.Context, pool db.DBPool, h *Hackathon, toPhase string) ([]BlockingReason, error) {
	var reasons []BlockingReason

	switch toPhase {
	case "results_published":
		// §6 exists so a contributor can contest a verdict. Publishing while
		// some PRs have not been judged would start that clock for people
		// whose result does not exist yet.
		var unjudged int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_verdicts
WHERE hackathon_id = $1
  AND prefilter_status <> 'rejected'
  AND final_bucket IS NULL
`, h.ID).Scan(&unjudged); err != nil {
			return nil, fmt.Errorf("hackathon.dynamicBlockers: count unjudged: %w", err)
		}
		if unjudged > 0 {
			reasons = append(reasons, BlockingReason{
				"verdicts",
				fmt.Sprintf("%d submission(s) still have no final bucket. Publish results only once every qualifying PR has been judged.", unjudged),
			})
		}

	case "settled":
		if h.ResultsPublishedAt == nil {
			break // already reported by requiredForTransition
		}
		window, err := GetAppealWindow(ctx, pool, h)
		if err != nil {
			return nil, err
		}
		if window.Open {
			reasons = append(reasons, BlockingReason{
				"appeal_window",
				fmt.Sprintf("The appeal window is still open until %s. Payouts release at Phase 6, after it closes.", window.ClosesAt.Format(time.RFC3339)),
			})
		}
		var pending int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_appeals WHERE hackathon_id = $1 AND status = 'pending'
`, h.ID).Scan(&pending); err != nil {
			return nil, fmt.Errorf("hackathon.dynamicBlockers: count pending appeals: %w", err)
		}
		if pending > 0 {
			reasons = append(reasons, BlockingReason{
				"appeals",
				fmt.Sprintf("%d appeal(s) are still awaiting a decision. Every appeal gets a human answer before anyone is paid.", pending),
			})
		}
	}
	return reasons, nil
}

// Readiness reports what's blocking hackathonID's next phase transition. The
// returned nextPhase is "" if the hackathon is already at the final phase
// implemented so far ("closed").
func Readiness(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (blocking []BlockingReason, nextPhase string, err error) {
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return nil, "", err
	}
	idx := phaseIndex(h.Phase)
	if idx < 0 || idx == len(PhaseOrder)-1 {
		return nil, "", nil
	}
	next := PhaseOrder[idx+1]
	blocking = requiredForTransition(h, next)
	dyn, err := dynamicBlockers(ctx, pool, h, next)
	if err != nil {
		return nil, "", err
	}
	return append(blocking, dyn...), next, nil
}

// Transition moves hackathonID to toPhase, enforcing sequential-only
// movement (no skipping, no backward), re-validating readiness, writing the
// phase change, and recording it in config_audit (key="phase"). On the
// issue_prep -> live transition specifically, it also snapshots every
// effective config value onto hackathons.config_snapshot - see AI-specs.md
// §1.1: from that point the running hackathon reads its snapshot, not live
// global config, so every assignment/verdict stays reproducible for an
// appeal even if an admin changes global defaults afterward.
func Transition(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, toPhase string, actorID uuid.UUID) error {
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return err
	}
	curIdx := phaseIndex(h.Phase)
	toIdx := phaseIndex(toPhase)
	if toIdx < 0 {
		return fmt.Errorf("hackathon.Transition: unknown phase %q", toPhase)
	}
	if toIdx != curIdx+1 {
		return fmt.Errorf("hackathon.Transition: cannot move from %q to %q - phases must advance one step at a time", h.Phase, toPhase)
	}
	if blocking := requiredForTransition(h, toPhase); len(blocking) > 0 {
		return fmt.Errorf("hackathon.Transition: %s", blocking[0].Message)
	}
	dyn, err := dynamicBlockers(ctx, pool, h, toPhase)
	if err != nil {
		return err
	}
	if len(dyn) > 0 {
		return fmt.Errorf("hackathon.Transition: %s", dyn[0].Message)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("hackathon.Transition: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `UPDATE hackathons SET phase = $1, updated_at = now() WHERE id = $2`, toPhase, hackathonID); err != nil {
		return fmt.Errorf("hackathon.Transition: update phase: %w", err)
	}

	hid := hackathonID
	if _, err := tx.Exec(ctx, `
INSERT INTO config_audit (hackathon_id, key, old_value, new_value, actor_user_id)
VALUES ($1, 'phase', $2, $3, $4)
`, hid, h.Phase, toPhase, actorID); err != nil {
		return fmt.Errorf("hackathon.Transition: write audit: %w", err)
	}

	// The appeal window is anchored to the moment a human published results.
	if toPhase == "results_published" {
		if _, err := tx.Exec(ctx, `
UPDATE hackathons SET results_published_at = now() WHERE id = $1 AND results_published_at IS NULL
`, hackathonID); err != nil {
			return fmt.Errorf("hackathon.Transition: stamp results_published_at: %w", err)
		}
	}

	if h.Phase == "issue_prep" && toPhase == "live" {
		values, err := EffectiveValues(ctx, pool, &hid)
		if err != nil {
			return fmt.Errorf("hackathon.Transition: compute config snapshot: %w", err)
		}
		snapshot, err := json.Marshal(values)
		if err != nil {
			return fmt.Errorf("hackathon.Transition: marshal config snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE hackathons SET config_snapshot = $1, config_snapshot_taken_at = now() WHERE id = $2
`, snapshot, hackathonID); err != nil {
			return fmt.Errorf("hackathon.Transition: write config snapshot: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("hackathon.Transition: commit: %w", err)
	}

	// Closing releases every in-flight assignment (AI-specs.md §13 #2).
	// Deliberately after the commit, not inside the transaction: the phase
	// change is the admin's action and must land even if releasing a
	// hundred assignments hits a problem. A failure here is logged by the
	// caller and re-converges on the next reconciler tick, which re-checks
	// closed hackathons for stragglers.
	if toPhase == "closed" {
		if _, err := CloseEventAssignments(ctx, pool, hackathonID); err != nil {
			return fmt.Errorf("hackathon.Transition: phase committed, but releasing in-flight assignments failed: %w", err)
		}
	}

	// §13-#4: a successful appeal recomputes unit_value for everyone, once at
	// appeal-window close rather than per appeal - otherwise the payouts stop
	// summing to the advertised pool. Settling is that close. Deliberately
	// after the commit and idempotent on appeals_closed_at, for the same
	// reason as CloseEventAssignments above: the admin's phase change must
	// land, and a failure here is retryable without paying anyone twice.
	if toPhase == "settled" {
		if _, err := CloseAppealsAndRecompute(ctx, pool, hackathonID, actorID); err != nil {
			return fmt.Errorf("hackathon.Transition: phase committed, but the post-appeal payout recompute failed: %w", err)
		}
	}
	return nil
}
