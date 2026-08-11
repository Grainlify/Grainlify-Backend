package hackathon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrFoundingWaveLocked is returned when someone tries to change a wave
// boundary after the first member has been assigned.
var ErrFoundingWaveLocked = errors.New("founding wave boundaries are locked once the first member has been assigned")

// foundingWaveLockedKeys are the config keys frozen at first assignment, and
// the column each is frozen into.
var foundingWaveLockedKeys = map[string]string{
	"founding_wave_founding_slots": "founding_slots::text",
	"founding_wave_two_slots":      "wave_two_slots::text",
	"founding_multiplier_founding": "multiplier_founding::text",
	"founding_multiplier_wave_two": "multiplier_wave_two::text",
	"founding_multiplier_open":     "multiplier_open::text",
}

// guardFoundingWaveBoundary refuses an edit that would move an announced wave
// boundary.
//
// The redesign is explicit that this must be blocked in code rather than
// merely discouraged. The first cohort joined *because* the tier was scarce;
// widening it afterwards tells them the scarcity was theatre, and tells
// everyone else that Grainlify's announced limits are not real. That is
// unusually expensive on this platform, because the whole anti-farming design
// - draw weights, frozen rules, published pool sizes - depends on published
// rules being believed. If a wave fills faster than expected, the answer is to
// add a wave below it, never to widen one already announced.
//
// Refusing at the point of the edit matters as much as refusing at assignment.
// Assignment already reads the locked row, so a changed config would simply be
// ignored - and a setting that appears to save but does nothing is worse than
// one that refuses, because the admin believes they changed something.
//
// A no-op write of the identical value is allowed: re-saving a form should not
// fail.
func guardFoundingWaveBoundary(ctx context.Context, exec pgExecutor, key, newValue string) error {
	column, guarded := foundingWaveLockedKeys[key]
	if !guarded {
		return nil
	}

	var locked string
	err := exec.QueryRow(ctx,
		`SELECT `+column+` FROM founding_wave_lock WHERE id = true`).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nobody assigned yet, so the boundaries are still open
	}
	if err != nil {
		// A missing table (before this migration runs) must not block config
		// edits that have nothing to do with the founding pool.
		return nil
	}

	if !numericallyEqual(locked, newValue) {
		return fmt.Errorf("%w: %s is locked at %s. To grow the programme, add a wave below the current one rather than widening one that has been announced",
			ErrFoundingWaveLocked, key, locked)
	}
	return nil
}

// numericallyEqual compares two numeric config strings by value rather than
// text, so "1.5" and "1.500" - which Postgres NUMERIC round-trips into - do
// not read as a change.
func numericallyEqual(a, b string) bool {
	if a == b {
		return true
	}
	return atofOr(a, -1) == atofOr(b, -2)
}
