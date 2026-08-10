package hackathon

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestShadowMode_FailsSafe pins the direction these defaults fail in.
// Publishing verdicts and paying out on an event meant to be a dry run
// cannot be taken back; the opposite failure costs an admin one click.
func TestShadowMode_FailsSafe(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, _, _ := fxLiveHackathon(t, pool)

	if !ShadowMode(ctx, pool, hackathonID) {
		t.Error("a hackathon with no explicit setting is not in shadow mode")
	}
	// A hackathon that doesn't exist at all must not read as live.
	if !ShadowMode(ctx, pool, uuid.New()) {
		t.Error("an unknown hackathon is not in shadow mode")
	}

	for _, v := range []string{"", "no", "0", "FALSE", "nonsense"} {
		fxSetConfig(t, pool, hackathonID, "judging_shadow_mode", v)
		if !ShadowMode(ctx, pool, hackathonID) {
			t.Errorf("value %q left shadow mode - only an exact \"false\" should", v)
		}
	}

	fxSetConfig(t, pool, hackathonID, "judging_shadow_mode", "false")
	if ShadowMode(ctx, pool, hackathonID) {
		t.Error("an explicit false did not leave shadow mode")
	}
}

func TestAIJudgingEnabled_DefaultsOff(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, _, _ := fxLiveHackathon(t, pool)

	if AIJudgingEnabled(ctx, pool, hackathonID) {
		t.Error("AI judging is on by default")
	}
	if AIJudgingEnabled(ctx, pool, uuid.New()) {
		t.Error("AI judging is on for an unknown hackathon")
	}
	for _, v := range []string{"", "yes", "1", "TRUE"} {
		fxSetConfig(t, pool, hackathonID, "ai_judging_enabled", v)
		if AIJudgingEnabled(ctx, pool, hackathonID) {
			t.Errorf("value %q enabled AI judging - only an exact \"true\" should", v)
		}
	}
	fxSetConfig(t, pool, hackathonID, "ai_judging_enabled", "true")
	if !AIJudgingEnabled(ctx, pool, hackathonID) {
		t.Error("an explicit true did not enable AI judging")
	}
}

// TestGuardPayoutRelease covers the rule that a verdict row - however
// complete - never releases money by itself.
func TestGuardPayoutRelease(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, _, _ := fxLiveHackathon(t, pool)
	admin := fxAdmin(t, pool)
	runID := uuid.New()

	full := PayoutReleaseRequest{
		HackathonID: hackathonID, PayoutRunID: runID, ActorID: admin, Confirm: true,
	}

	// Shadow mode blocks it even when everything else is in order.
	if err := GuardPayoutRelease(ctx, pool, full); !errors.Is(err, ErrPayoutNotReleasable) {
		t.Errorf("shadow mode did not block the release: %v", err)
	}

	fxSetConfig(t, pool, hackathonID, "judging_shadow_mode", "false")

	// Outside shadow mode it still needs every explicit part.
	tests := []struct {
		name   string
		mutate func(*PayoutReleaseRequest)
	}{
		{"unconfirmed", func(r *PayoutReleaseRequest) { r.Confirm = false }},
		{"no admin actor", func(r *PayoutReleaseRequest) { r.ActorID = uuid.Nil }},
		{"no computed run", func(r *PayoutReleaseRequest) { r.PayoutRunID = uuid.Nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := full
			tc.mutate(&req)
			if err := GuardPayoutRelease(ctx, pool, req); !errors.Is(err, ErrPayoutNotReleasable) {
				t.Errorf("released with %s: %v", tc.name, err)
			}
		})
	}

	// Outside shadow mode and fully explicit is still not enough: §6 puts
	// payout release at Phase 6, after the appeal window closes. A hackathon
	// that has not settled has not finished deciding what anyone is owed.
	if err := GuardPayoutRelease(ctx, pool, full); !errors.Is(err, ErrPayoutNotReleasable) {
		t.Errorf("released before the hackathon settled: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE hackathons SET phase = 'settled' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("force settled: %v", err)
	}

	// Settled but with no appeals_closed_at means the §13-#4 recompute never
	// ran, so any stored unit_value predates the appeal decisions.
	if err := GuardPayoutRelease(ctx, pool, full); !errors.Is(err, ErrPayoutNotReleasable) {
		t.Errorf("released with the appeal window still un-closed: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE hackathons SET appeals_closed_at = now() WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("close the appeal window: %v", err)
	}

	// Only now: explicit, out of shadow mode, settled, and recomputed.
	if err := GuardPayoutRelease(ctx, pool, full); err != nil {
		t.Errorf("a fully confirmed admin release on a settled, recomputed hackathon was blocked: %v", err)
	}
}
