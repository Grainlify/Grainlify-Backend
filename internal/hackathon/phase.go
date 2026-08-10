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
// results_published/settled (AI-specs.md §1's Phase 5-6) belong to the
// judging/payout slice and will extend this list, not replace it.
var PhaseOrder = []string{"draft", "application_period", "issue_prep", "live", "closed"}

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
}

func loadHackathon(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (*Hackathon, error) {
	var h Hackathon
	err := pool.QueryRow(ctx, `
SELECT id, phase, announced_at, application_period_start, application_period_end, issue_prep_start, starts_at, ends_at
FROM hackathons WHERE id = $1
`, hackathonID).Scan(&h.ID, &h.Phase, &h.AnnouncedAt, &h.ApplicationPeriodStart, &h.ApplicationPeriodEnd, &h.IssuePrepStart, &h.StartsAt, &h.EndsAt)
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
	}
	return reasons
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
	return requiredForTransition(h, next), next, nil
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
		return fmt.Errorf("hackathon.Transition: %d requirement(s) not met for %q", len(blocking), toPhase)
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
	return nil
}
