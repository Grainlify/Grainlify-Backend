package hackathon

import (
	"context"
	"errors"
	"fmt"

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
// Two independent conditions, both required:
//
//  1. An admin explicitly asked for it. A verdict row existing - even a
//     complete, confident, cross-checked one - never releases money by
//     itself. Judging decides *what* a contribution was worth; a human
//     decides *when* to pay it.
//  2. The event is not in shadow mode.
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
	return nil
}
