package hackathon

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func TestEffectiveValue_ResolutionOrder(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})

	key := "max_issues_per_org"
	// hackathon_config_settings' global (hackathon_id IS NULL) scope is
	// shared, un-isolated state across the whole test suite - unlike a
	// fresh hackathonID, there's only ever one global row per key. Clean up
	// this test's own global mutation so it can't leak into any other test
	// that reads this key's global/factory-default value (hit this for
	// real: TestEffectiveValues_MergesGlobalAndOverrideAcrossEveryDefinedKey
	// failed until this cleanup was added).
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM hackathon_config_settings WHERE hackathon_id IS NULL AND key = $1`, key)
	})

	// 1. No override anywhere - factory default.
	got, err := EffectiveValue(ctx, d.Pool, &hackathonID, key)
	if err != nil {
		t.Fatalf("EffectiveValue: %v", err)
	}
	if got != DefaultValue(key) {
		t.Errorf("factory default: got %q, want %q", got, DefaultValue(key))
	}

	// 2. Global override set - beats factory default.
	if err := SetValue(ctx, d.Pool, nil, key, "77", actor); err != nil {
		t.Fatalf("SetValue(global): %v", err)
	}
	got, err = EffectiveValue(ctx, d.Pool, &hackathonID, key)
	if err != nil {
		t.Fatalf("EffectiveValue: %v", err)
	}
	if got != "77" {
		t.Errorf("after global override: got %q, want 77", got)
	}

	// 3. Per-hackathon override set - beats global.
	if err := SetValue(ctx, d.Pool, &hackathonID, key, "12", actor); err != nil {
		t.Fatalf("SetValue(hackathon): %v", err)
	}
	got, err = EffectiveValue(ctx, d.Pool, &hackathonID, key)
	if err != nil {
		t.Fatalf("EffectiveValue: %v", err)
	}
	if got != "12" {
		t.Errorf("after hackathon override: got %q, want 12", got)
	}

	// A different hackathon never sees this one's override, only the global.
	otherHackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})
	got, err = EffectiveValue(ctx, d.Pool, &otherHackathonID, key)
	if err != nil {
		t.Fatalf("EffectiveValue(other): %v", err)
	}
	if got != "77" {
		t.Errorf("other hackathon: got %q, want 77 (the global override)", got)
	}
}

func TestSetValue_WritesAuditRow(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})
	key := "late_entry_cutoff_hours"

	if err := SetValue(ctx, d.Pool, &hackathonID, key, "24", actor); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	var oldValue, newValue string
	var gotActor uuid.UUID
	var gotHackathon uuid.UUID
	err := d.Pool.QueryRow(ctx, `
SELECT old_value, new_value, actor_user_id, hackathon_id FROM config_audit
WHERE hackathon_id = $1 AND key = $2 ORDER BY created_at DESC LIMIT 1
`, hackathonID, key).Scan(&oldValue, &newValue, &gotActor, &gotHackathon)
	if err != nil {
		t.Fatalf("query audit row: %v", err)
	}
	if oldValue != DefaultValue(key) {
		t.Errorf("audit old_value = %q, want factory default %q (first-ever override)", oldValue, DefaultValue(key))
	}
	if newValue != "24" {
		t.Errorf("audit new_value = %q, want 24", newValue)
	}
	if gotActor != actor {
		t.Errorf("audit actor = %v, want %v", gotActor, actor)
	}
	if gotHackathon != hackathonID {
		t.Errorf("audit hackathon_id = %v, want %v", gotHackathon, hackathonID)
	}

	// A second change picks up the real prior value as old_value, not the
	// factory default again.
	if err := SetValue(ctx, d.Pool, &hackathonID, key, "36", actor); err != nil {
		t.Fatalf("SetValue (2nd): %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
SELECT old_value, new_value FROM config_audit
WHERE hackathon_id = $1 AND key = $2 ORDER BY created_at DESC LIMIT 1
`, hackathonID, key).Scan(&oldValue, &newValue); err != nil {
		t.Fatalf("query audit row (2nd): %v", err)
	}
	if oldValue != "24" || newValue != "36" {
		t.Errorf("2nd audit row = (%q -> %q), want (24 -> 36)", oldValue, newValue)
	}
}

func TestSetValue_UnknownKeyRejected(t *testing.T) {
	d := dbtest.DB(t)
	actor := fxUser(t, d.Pool)
	if err := SetValue(context.Background(), d.Pool, nil, "not_a_real_key", "x", actor); err == nil {
		t.Error("SetValue with an unknown key should fail, got nil error")
	}
}

func TestResetValue_FallsBackToGlobal(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})
	key := "max_issues_per_org"
	// Same shared-global-scope leak risk as TestEffectiveValue_ResolutionOrder
	// above - this test also writes a global override for this key, and
	// ResetValue below only clears the per-hackathon row, not this one.
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM hackathon_config_settings WHERE hackathon_id IS NULL AND key = $1`, key)
	})

	if err := SetValue(ctx, d.Pool, nil, key, "77", actor); err != nil {
		t.Fatalf("SetValue(global): %v", err)
	}
	if err := SetValue(ctx, d.Pool, &hackathonID, key, "12", actor); err != nil {
		t.Fatalf("SetValue(hackathon): %v", err)
	}

	if err := ResetValue(ctx, d.Pool, hackathonID, key, actor); err != nil {
		t.Fatalf("ResetValue: %v", err)
	}

	got, err := EffectiveValue(ctx, d.Pool, &hackathonID, key)
	if err != nil {
		t.Fatalf("EffectiveValue: %v", err)
	}
	if got != "77" {
		t.Errorf("after reset: got %q, want 77 (the global value)", got)
	}

	var newValue string
	if err := d.Pool.QueryRow(ctx, `
SELECT new_value FROM config_audit WHERE hackathon_id = $1 AND key = $2 ORDER BY created_at DESC LIMIT 1
`, hackathonID, key).Scan(&newValue); err != nil {
		t.Fatalf("query audit row: %v", err)
	}
	if newValue != "77" {
		t.Errorf("reset audit new_value = %q, want 77", newValue)
	}
}

func TestResetValue_NoOpWhenNoOverrideExists(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})

	// Should not error, and should not write a spurious audit row.
	if err := ResetValue(ctx, d.Pool, hackathonID, "max_issues_per_org", actor); err != nil {
		t.Fatalf("ResetValue on a key with no override: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM config_audit WHERE hackathon_id = $1`, hackathonID).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no audit rows written for a no-op reset, got %d", count)
	}
}

func TestEffectiveValues_MergesGlobalAndOverrideAcrossEveryDefinedKey(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{})

	if err := SetValue(ctx, d.Pool, &hackathonID, "grainhack_label", "custom-label", actor); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	values, err := EffectiveValues(ctx, d.Pool, &hackathonID)
	if err != nil {
		t.Fatalf("EffectiveValues: %v", err)
	}
	if len(values) != len(Definitions) {
		t.Errorf("EffectiveValues returned %d keys, want %d (one per Definitions entry)", len(values), len(Definitions))
	}
	if values["grainhack_label"] != "custom-label" {
		t.Errorf("grainhack_label = %q, want custom-label", values["grainhack_label"])
	}
	// Cross-check a key this test never touched against a direct
	// EffectiveValue call instead of assuming a hardcoded factory default -
	// hackathon_config_settings' global scope is shared, un-isolated state
	// across the whole suite, so another test may have its own global
	// override in place concurrently/previously. What matters here is that
	// EffectiveValues and EffectiveValue agree, not any specific value.
	untouchedKey := "max_issues_per_org"
	wantUntouched, err := EffectiveValue(ctx, d.Pool, &hackathonID, untouchedKey)
	if err != nil {
		t.Fatalf("EffectiveValue(%s): %v", untouchedKey, err)
	}
	if values[untouchedKey] != wantUntouched {
		t.Errorf("%s = %q, want %q (EffectiveValues must agree with EffectiveValue for an untouched key)", untouchedKey, values[untouchedKey], wantUntouched)
	}
}
