package hackathon

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestSetValue_RefusesWaveBoundaryEditAfterLock is the admin-facing half of
// the wave lock. Assignment already reads the frozen row, so an accepted-but-
// ignored edit would be worse than a refusal: the admin would believe they
// had changed something.
func TestSetValue_RefusesWaveBoundaryEditAfterLock(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	var actor uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email, role) VALUES ($1, 'admin') RETURNING id`,
		"wave-guard-"+uuid.NewString()+"@example.com").Scan(&actor); err != nil {
		t.Fatalf("create actor: %v", err)
	}

	if _, err := d.Pool.Exec(ctx, `TRUNCATE founding_wave_lock`); err != nil {
		t.Fatalf("reset lock: %v", err)
	}

	// Before anyone is assigned the boundaries are still editable.
	if err := SetValue(ctx, d.Pool, nil, "founding_wave_founding_slots", "150", actor); err != nil {
		t.Fatalf("editing before the lock should be allowed: %v", err)
	}

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO founding_wave_lock
  (id, founding_slots, wave_two_slots, multiplier_founding, multiplier_wave_two, multiplier_open)
VALUES (true, 100, 400, 1.5, 1.25, 1.0)
`); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	if err := SetValue(ctx, d.Pool, nil, "founding_wave_founding_slots", "500", actor); err == nil {
		t.Error("widening the Founding wave after the lock was accepted; it must be refused")
	}

	// Re-saving the identical value is not a change and must not fail - a
	// settings form that cannot be submitted twice is its own bug.
	if err := SetValue(ctx, d.Pool, nil, "founding_wave_founding_slots", "100", actor); err != nil {
		t.Errorf("re-saving the locked value failed: %v", err)
	}
	if err := SetValue(ctx, d.Pool, nil, "founding_multiplier_founding", "1.500", actor); err != nil {
		t.Errorf("re-saving a numerically identical multiplier failed: %v", err)
	}

	// Keys outside the lock are unaffected.
	if err := SetValue(ctx, d.Pool, nil, "founding_pool_usdc", "5000", actor); err != nil {
		t.Errorf("editing the pool size should stay allowed: %v", err)
	}
}
