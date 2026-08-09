package hackathon

import (
	"context"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func TestEffectiveGrainHackLabels_NoAcceptedApplication(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")

	labels, err := EffectiveGrainHackLabels(ctx, d.Pool, projectID)
	if err != nil {
		t.Fatalf("EffectiveGrainHackLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != LegacyGrainHackLabel {
		t.Errorf("labels = %v, want just [%q] (no accepted application anywhere)", labels, LegacyGrainHackLabel)
	}
}

func TestEffectiveGrainHackLabels_WidensWithConfiguredLabel(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	admin := fxAdmin(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	if err := SetValue(ctx, d.Pool, &hackathonID, "grainhack_label", "hack-2026", admin); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	labels, err := EffectiveGrainHackLabels(ctx, d.Pool, projectID)
	if err != nil {
		t.Fatalf("EffectiveGrainHackLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("labels = %v, want 2 entries (legacy literal + configured label)", labels)
	}
	found := map[string]bool{}
	for _, l := range labels {
		found[l] = true
	}
	if !found[LegacyGrainHackLabel] {
		t.Errorf("labels %v missing the legacy literal %q - must never be dropped", labels, LegacyGrainHackLabel)
	}
	if !found["hack-2026"] {
		t.Errorf("labels %v missing the configured hackathon label", labels)
	}
}

func TestEffectiveGrainHackLabels_DedupesWhenConfiguredLabelMatchesLegacy(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "live"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)
	// grainhack_label's factory default is "grainhack" (lowercase), which
	// case-insensitively equals the legacy literal "GrainHack" - the common
	// case for any hackathon that never customizes the label.

	labels, err := EffectiveGrainHackLabels(ctx, d.Pool, projectID)
	if err != nil {
		t.Fatalf("EffectiveGrainHackLabels: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("labels = %v, want exactly 1 entry (default config label case-insensitively equals the legacy literal)", labels)
	}
}

func TestEffectiveGrainHackLabels_IgnoresNonAcceptedApplication(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	admin := fxAdmin(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})

	// A pending (not accepted) application must not widen the label set.
	_, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, 'd', 'g', 1, 'c', 'pending')
`, hackathonID, projectID, owner)
	if err != nil {
		t.Fatalf("insert pending application: %v", err)
	}
	if err := SetValue(ctx, d.Pool, &hackathonID, "grainhack_label", "hack-2026", admin); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	labels, err := EffectiveGrainHackLabels(ctx, d.Pool, projectID)
	if err != nil {
		t.Fatalf("EffectiveGrainHackLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != LegacyGrainHackLabel {
		t.Errorf("labels = %v, want just [%q] (application is pending, not accepted)", labels, LegacyGrainHackLabel)
	}
}

func TestHasLabel_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name       string
		issue      []string
		candidates []string
		want       bool
	}{
		{"exact match", []string{"bug", "GrainHack"}, []string{"GrainHack"}, true},
		{"case-insensitive match", []string{"bug", "grainhack"}, []string{"GrainHack"}, true},
		{"no match", []string{"bug", "priority:high"}, []string{"GrainHack"}, false},
		{"empty issue labels", nil, []string{"GrainHack"}, false},
		{"matches second candidate", []string{"hack-2026"}, []string{"GrainHack", "hack-2026"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasLabel(tt.issue, tt.candidates); got != tt.want {
				t.Errorf("HasLabel(%v, %v) = %v, want %v", tt.issue, tt.candidates, got, tt.want)
			}
		})
	}
}
