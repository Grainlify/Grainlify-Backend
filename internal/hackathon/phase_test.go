package hackathon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func TestReadiness_ReportsSpecificBlockingReasons(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "draft"})

	blocking, nextPhase, err := Readiness(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if nextPhase != "application_period" {
		t.Errorf("nextPhase = %q, want application_period", nextPhase)
	}
	if len(blocking) != 3 {
		t.Fatalf("expected 3 blocking reasons (announced_at, application_period_start, application_period_end), got %d: %+v", len(blocking), blocking)
	}
	fields := map[string]bool{}
	for _, b := range blocking {
		fields[b.Field] = true
	}
	for _, want := range []string{"announced_at", "application_period_start", "application_period_end"} {
		if !fields[want] {
			t.Errorf("missing blocking reason for field %q", want)
		}
	}

	// Fill in the required fields directly - Readiness should then report
	// no blockers for this transition.
	now := time.Now()
	_, err = d.Pool.Exec(ctx, `
UPDATE hackathons SET announced_at = $1, application_period_start = $1, application_period_end = $2 WHERE id = $3
`, now, now.Add(24*time.Hour), hackathonID)
	if err != nil {
		t.Fatalf("update hackathon: %v", err)
	}
	blocking, _, err = Readiness(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("Readiness (after fill): %v", err)
	}
	if len(blocking) != 0 {
		t.Errorf("expected no blocking reasons once required fields are set, got %+v", blocking)
	}
}

func TestReadiness_AtFinalPhaseReturnsNoNextPhase(t *testing.T) {
	d := dbtest.DB(t)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "live"})

	blocking, nextPhase, err := Readiness(context.Background(), d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if nextPhase != "" {
		t.Errorf("nextPhase = %q, want empty (already at the final phase this slice implements)", nextPhase)
	}
	if blocking != nil {
		t.Errorf("blocking = %+v, want nil", blocking)
	}
}

func TestTransition_SequentialOnly(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "draft"})

	// Cannot skip ahead.
	if err := Transition(ctx, d.Pool, hackathonID, "issue_prep", actor); err == nil {
		t.Error("Transition draft -> issue_prep (skipping application_period) should fail, got nil error")
	}

	// Cannot move backward (from draft, "draft" itself isn't forward).
	if err := Transition(ctx, d.Pool, hackathonID, "draft", actor); err == nil {
		t.Error("Transition draft -> draft should fail (not a forward move), got nil error")
	}

	// Correct one-step-forward transition still blocked by missing required fields.
	if err := Transition(ctx, d.Pool, hackathonID, "application_period", actor); err == nil {
		t.Error("Transition to application_period with no required fields set should fail, got nil error")
	}

	now := time.Now()
	if _, err := d.Pool.Exec(ctx, `
UPDATE hackathons SET announced_at = $1, application_period_start = $1, application_period_end = $2 WHERE id = $3
`, now, now.Add(24*time.Hour), hackathonID); err != nil {
		t.Fatalf("update hackathon: %v", err)
	}

	if err := Transition(ctx, d.Pool, hackathonID, "application_period", actor); err != nil {
		t.Fatalf("Transition draft -> application_period (fields set): %v", err)
	}

	var phase string
	if err := d.Pool.QueryRow(ctx, `SELECT phase FROM hackathons WHERE id = $1`, hackathonID).Scan(&phase); err != nil {
		t.Fatalf("query phase: %v", err)
	}
	if phase != "application_period" {
		t.Errorf("phase = %q, want application_period", phase)
	}

	// A phase-transition audit row was written with key='phase'.
	var oldValue, newValue string
	if err := d.Pool.QueryRow(ctx, `
SELECT old_value, new_value FROM config_audit WHERE hackathon_id = $1 AND key = 'phase' ORDER BY created_at DESC LIMIT 1
`, hackathonID).Scan(&oldValue, &newValue); err != nil {
		t.Fatalf("query phase audit row: %v", err)
	}
	if oldValue != "draft" || newValue != "application_period" {
		t.Errorf("phase audit row = (%q -> %q), want (draft -> application_period)", oldValue, newValue)
	}
}

func TestTransition_UnknownPhaseRejected(t *testing.T) {
	d := dbtest.DB(t)
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "draft"})

	if err := Transition(context.Background(), d.Pool, hackathonID, "not_a_real_phase", actor); err == nil {
		t.Error("Transition to an unknown phase should fail, got nil error")
	}
}

func TestTransition_IssuePrepToLive_SnapshotsConfig(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	actor := fxUser(t, d.Pool)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})

	// Set a distinctive per-hackathon override before going live, so the
	// snapshot can be verified to actually contain it (not just any JSON).
	if err := SetValue(ctx, d.Pool, &hackathonID, "max_issues_per_org", "17", actor); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	now := time.Now()
	if _, err := d.Pool.Exec(ctx, `UPDATE hackathons SET starts_at = $1, ends_at = $2 WHERE id = $3`, now, now.Add(48*time.Hour), hackathonID); err != nil {
		t.Fatalf("update hackathon: %v", err)
	}

	if err := Transition(ctx, d.Pool, hackathonID, "live", actor); err != nil {
		t.Fatalf("Transition issue_prep -> live: %v", err)
	}

	var snapshotJSON []byte
	var snapshotTakenAt *time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT config_snapshot, config_snapshot_taken_at FROM hackathons WHERE id = $1`, hackathonID).Scan(&snapshotJSON, &snapshotTakenAt); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if snapshotTakenAt == nil {
		t.Fatal("config_snapshot_taken_at is nil, want a real timestamp")
	}
	var snapshot map[string]string
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snapshot["max_issues_per_org"] != "17" {
		t.Errorf("snapshot[max_issues_per_org] = %q, want 17", snapshot["max_issues_per_org"])
	}
	if len(snapshot) != len(Definitions) {
		t.Errorf("snapshot has %d keys, want %d (one per Definitions entry)", len(snapshot), len(Definitions))
	}

	// Changing the global default AFTER going live must not retroactively
	// alter the already-taken snapshot (AI-specs.md §1.1's whole point).
	if err := SetValue(ctx, d.Pool, nil, "max_issues_per_org", "999", actor); err != nil {
		t.Fatalf("SetValue(global, post-live): %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM hackathon_config_settings WHERE hackathon_id IS NULL AND key = 'max_issues_per_org'`)
	})

	var snapshotAfterJSON []byte
	if err := d.Pool.QueryRow(ctx, `SELECT config_snapshot FROM hackathons WHERE id = $1`, hackathonID).Scan(&snapshotAfterJSON); err != nil {
		t.Fatalf("query snapshot (after global change): %v", err)
	}
	var snapshotAfter map[string]string
	if err := json.Unmarshal(snapshotAfterJSON, &snapshotAfter); err != nil {
		t.Fatalf("unmarshal snapshot (after): %v", err)
	}
	if snapshotAfter["max_issues_per_org"] != "17" {
		t.Errorf("snapshot[max_issues_per_org] after a post-live global change = %q, want unchanged 17", snapshotAfter["max_issues_per_org"])
	}
}
