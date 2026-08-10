package hackathon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ErrPayoutNotReleasable is returned when something tries to move money
// without the explicit admin action that releasing a payout requires.
var ErrPayoutNotReleasable = errors.New("payout release requires an explicit admin action")

// ShadowMode reports whether this hackathon computes verdicts without
// publishing them (AI-specs.md §12: "Event 1 - shadow mode. Assignment runs
// live. Judging runs but publishes nothing; pay by hand.").
//
// **Fails safe.** Any error - missing config, unreachable DB, a malformed
// value - returns true. Getting this backwards means publishing verdicts and
// paying out on an event that was supposed to be a dry run, which is not
// something you can take back; the opposite failure just means an admin has
// to flip a switch.
func ShadowMode(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) bool {
	v, err := EffectiveValue(ctx, pool, &hackathonID, "judging_shadow_mode")
	if err != nil {
		return true
	}
	// Only an explicit "false" leaves shadow mode. An empty or unrecognised
	// value stays in it.
	return v != "false"
}

// AIJudgingEnabled reports whether §5's model stages (3-5) may run. Fails
// safe in the other direction for the same reason: not running the model is
// recoverable, spending on it unintentionally is not.
func AIJudgingEnabled(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) bool {
	v, err := EffectiveValue(ctx, pool, &hackathonID, "ai_judging_enabled")
	if err != nil {
		return false
	}
	return v == "true"
}

// PayoutReleaseRequest is the explicit admin action that releases a payout.
type PayoutReleaseRequest struct {
	HackathonID uuid.UUID
	PayoutRunID uuid.UUID
	ActorID     uuid.UUID
	// Confirm must be true. A required, explicitly-set field rather than an
	// inferred one, so a release can never be the accidental consequence of
	// calling something that sounded read-only.
	Confirm bool
}

// GuardPayoutRelease is the single chokepoint every payout release must pass.
//
// Four independent conditions, all required:
//
//  1. An admin explicitly asked for it. A verdict row existing - even a
//     complete, confident, cross-checked one - never releases money by
//     itself. Judging decides *what* a contribution was worth; a human
//     decides *when* to pay it.
//  2. The event is not in shadow mode.
//  3. The hackathon has reached Phase 6 (settled). §6: "Payouts release at
//     Phase 6, after the appeal window closes."
//  4. The appeal window has actually been closed out, which is also what
//     guarantees the §13-#4 recompute has run. Paying from a pre-appeal
//     payout run would pay the old unit_value - correct for nobody if any
//     appeal was upheld, since an upheld appeal changes total_units and
//     therefore everyone's share.
//
// Conditions 3 and 4 are separate on purpose. The phase is an admin's stated
// intent; appeals_closed_at is the record that the arithmetic was redone.
// A hackathon can be moved to settled while the recompute fails, and that
// combination must not pay anyone.
//
// Kept as a guard rather than folded into the release function so that any
// future release path has to call it, and so the rule is testable on its own.
func GuardPayoutRelease(ctx context.Context, pool db.DBPool, req PayoutReleaseRequest) error {
	if !req.Confirm {
		return fmt.Errorf("%w: not explicitly confirmed", ErrPayoutNotReleasable)
	}
	if req.ActorID == uuid.Nil {
		return fmt.Errorf("%w: no admin actor recorded", ErrPayoutNotReleasable)
	}
	if req.PayoutRunID == uuid.Nil {
		return fmt.Errorf("%w: no computed payout run to release", ErrPayoutNotReleasable)
	}
	if ShadowMode(ctx, pool, req.HackathonID) {
		return fmt.Errorf("%w: this hackathon is in shadow mode, which computes payouts but pays nothing", ErrPayoutNotReleasable)
	}

	var phase string
	var appealsClosedAt *time.Time
	err := pool.QueryRow(ctx, `
SELECT phase, appeals_closed_at FROM hackathons WHERE id = $1
`, req.HackathonID).Scan(&phase, &appealsClosedAt)
	if err != nil {
		// Fails closed: if we cannot establish that the appeal window is
		// closed, we have not established that it is safe to pay.
		return fmt.Errorf("%w: could not confirm the hackathon has settled: %v", ErrPayoutNotReleasable, err)
	}
	if phase != "settled" {
		return fmt.Errorf("%w: payouts release at Phase 6 (settled), after the appeal window closes - this hackathon is %q", ErrPayoutNotReleasable, phase)
	}
	if appealsClosedAt == nil {
		return fmt.Errorf("%w: the appeal window has not been closed out, so the post-appeal recompute has not run", ErrPayoutNotReleasable)
	}
	return nil
}
