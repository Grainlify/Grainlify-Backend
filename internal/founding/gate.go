package founding

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The social-follow gate, in one place, read by both the thing that lets you
// IN and the thing that PAYS you.
//
// # Why one definition and one key
//
// Entry (AssignWave) and payment (Eligible) are meant to be the same rule.
// Expressed twice they drift, and the drift is worst at the moment somebody
// flips the switch: a gate that is unconditional on the entry side while the
// payment side reads a flag would, when that flag is turned off, leave entry
// restricted and payment open - the two halves failing in opposite directions
// simultaneously, from a single deliberate action that looks like it should
// loosen things.
//
// So both sides call requireSocialFollow, both read
// founding_require_social_follow, and there is exactly one switch:
//
//	true (default)   entry requires an approved submission, and settlement
//	                 pays only approved submitters
//	false            entry is open to anyone who verifies, and settlement pays
//	                 everyone - the pre-gate behaviour, restored whole
//
// Note the asymmetry that remains and is intended: turning the flag off does
// NOT retroactively assign waves to people who were refused entry while it was
// on. They get a position the next time an assignment is attempted for them,
// which for a verified contributor means their next social-follow approval,
// and for an approved one means verification. Nothing re-runs history.

// ErrNotEligibleForWave means the contributor has no wave because they have
// not been approved, not because anything failed.
//
// A sentinel rather than a bare (zero, nil) so a caller cannot mistake "not
// assigned" for "assigned position 0", and so the log line can say which of
// the two happened. Callers must treat it as an ordinary outcome.
var ErrNotEligibleForWave = errors.New("not eligible for a wave: no approved social follow submission")

// requireSocialFollow reports whether the gate is on. Off only for the exact
// string "false", so an unset key - which is the live state, since no founding
// config rows exist - leaves the gate ON, matching the "true" default declared
// in internal/hackathon/config.go.
func requireSocialFollow(cfg map[string]string) bool {
	return cfg["founding_require_social_follow"] != "false"
}

// socialFollowState returns the stored submission status, or "" when there has
// never been one.
//
// Reads the submission's own status rather than any separate "completed"
// record: a row meaning "was paid" is not the same claim as "is eligible now",
// and would keep somebody eligible after their approval was withdrawn.
func socialFollowState(ctx context.Context, pool db.DBPool, userID uuid.UUID) (string, error) {
	var status string
	err := pool.QueryRow(ctx, `
SELECT status FROM social_follow_submissions WHERE user_id = $1
`, userID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("founding.socialFollowState: %w", err)
	}
	return status, nil
}

// approvedForFoundingPool is the single test both sides apply.
//
// Only 'approved' passes. 'revoked' is the case the distinction exists for: an
// approval since withdrawn must not confer eligibility, and it is the path
// most likely to break silently, because a revoked submission still looks like
// a submission.
func approvedForFoundingPool(ctx context.Context, pool db.DBPool, userID uuid.UUID, cfg map[string]string) (bool, string, error) {
	if !requireSocialFollow(cfg) {
		return true, "", nil
	}
	status, err := socialFollowState(ctx, pool, userID)
	if err != nil {
		return false, "", err
	}
	switch status {
	case "approved":
		return true, "", nil
	case "":
		return false, "no social follow submission", nil
	case "revoked":
		return false, "social follow approval was revoked", nil
	default:
		return false, "social follow not approved (" + status + ")", nil
	}
}
