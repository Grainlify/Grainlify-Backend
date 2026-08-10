package hackathon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/ai"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestAssessFit_DisabledScoresPlausible is the guarantee that makes the
// deterministic pipeline complete: with the flag off, every applicant that
// passed the gates is assessed "plausible" - a real §4.4 outcome, not a
// placeholder - so gates/tickets/draws are fully exercisable with zero
// model calls.
func TestAssessFit_DisabledScoresPlausible(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "ai_fit_assessment_enabled", "false")
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 300, "standard")

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "unassessed")
	appID := fxApplication(t, pool, hackathonID, issueID, userID, "unassessed", "")
	// Clear the fixture's fit so AssessFit is genuinely what sets it.
	if _, err := pool.Exec(ctx, `UPDATE hackathon_issue_applications SET fit = NULL, difficulty_match = NULL, fit_assessed_at = NULL WHERE id = $1`, appID); err != nil {
		t.Fatalf("clear fit: %v", err)
	}

	if err := AssessFit(ctx, pool, ai.NewClient(""), nil, "", appID); err != nil {
		t.Fatalf("AssessFit: %v", err)
	}

	var fit, diffMatch string
	var assessedAt *time.Time
	var model *string
	if err := pool.QueryRow(ctx, `
SELECT fit, difficulty_match, fit_assessed_at, fit_model FROM hackathon_issue_applications WHERE id = $1
`, appID).Scan(&fit, &diffMatch, &assessedAt, &model); err != nil {
		t.Fatalf("read fit: %v", err)
	}
	if fit != "plausible" {
		t.Errorf("fit = %q, want plausible", fit)
	}
	if diffMatch != "matched" {
		t.Errorf("difficulty_match = %q, want matched", diffMatch)
	}
	if assessedAt == nil {
		t.Error("fit_assessed_at not set - the draw needs to distinguish assessed from pending")
	}
	if model != nil {
		t.Errorf("fit_model = %v, want NULL when no model was called", *model)
	}
}

// Even with the flag on, no API key must not drop the applicant from the
// draw - it degrades to the same neutral assessment.
func TestAssessFit_EnabledButNoAPIKeyStillScores(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "ai_fit_assessment_enabled", "true")
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 301, "standard")

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "no-key")
	appID := fxApplication(t, pool, hackathonID, issueID, userID, "no-key", "")
	if _, err := pool.Exec(ctx, `UPDATE hackathon_issue_applications SET fit = NULL WHERE id = $1`, appID); err != nil {
		t.Fatalf("clear fit: %v", err)
	}

	if err := AssessFit(ctx, pool, ai.NewClient(""), nil, "", appID); err != nil {
		t.Fatalf("AssessFit: %v", err)
	}
	var fit string
	if err := pool.QueryRow(ctx, `SELECT fit FROM hackathon_issue_applications WHERE id = $1`, appID).Scan(&fit); err != nil {
		t.Fatalf("read fit: %v", err)
	}
	if fit != "plausible" {
		t.Errorf("fit = %q, want plausible - a missing key must not silently exclude an applicant", fit)
	}
}

// TestAssignmentSystemPrompt_KeepsFairnessRules guards the §4.4 text that
// stops the model defaulting to rewarding volume and confident prose. If
// someone trims the prompt, this fails loudly rather than quietly changing
// who wins draws.
func TestAssignmentSystemPrompt_KeepsFairnessRules(t *testing.T) {
	required := []string{
		"UNTRUSTED",
		"instruction_injection_attempt",
		"THIS IS THE CORRECT",
		"Absence of a long history is NOT weak fit",
		"Do NOT reward volume",
		"DIFFICULTY TIER",
	}
	for _, phrase := range required {
		if !strings.Contains(assignmentSystemPrompt, phrase) {
			t.Errorf("assignment prompt is missing the §4.4 clause %q", phrase)
		}
	}
}
