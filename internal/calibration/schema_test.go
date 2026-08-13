package calibration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// seedLabelRow inserts the minimum chain of rows needed to hold one label:
// a project, a PR, a sample, a sample PR, and a user to label with.
func seedLabelRow(t *testing.T, d *db.DB) (samplePRID, labellerID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "calib-owner-"+uuid.NewString()[:8]).Scan(&ownerID); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	var projectID uuid.UUID
	err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (github_full_name, github_repo_id, owner_user_id) VALUES ($1, $2, $3) RETURNING id
`, "calib/"+uuid.NewString()[:8], int64(uuid.New().ID()), ownerID).Scan(&projectID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	var prID uuid.UUID
	err = d.Pool.QueryRow(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, merged)
VALUES ($1, $2, $3, 'closed', 'a pull request', true) RETURNING id
`, projectID, int64(uuid.New().ID()), 1).Scan(&prID)
	if err != nil {
		t.Fatalf("insert pr: %v", err)
	}

	var sampleID uuid.UUID
	err = d.Pool.QueryRow(ctx, `
INSERT INTO calibration_samples (name, seed, candidate_pr_ids, candidate_hash, strata)
VALUES ($1, 42, $2, 'deadbeef', '{}'::jsonb) RETURNING id
`, "set-"+uuid.NewString()[:8], []uuid.UUID{prID}).Scan(&sampleID)
	if err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	err = d.Pool.QueryRow(ctx, `
INSERT INTO calibration_sample_prs (sample_id, pull_request_id, project_full_name, merged, size_band)
VALUES ($1, $2, 'calib/repo', true, 'small') RETURNING id
`, sampleID, prID).Scan(&samplePRID)
	if err != nil {
		t.Fatalf("insert sample pr: %v", err)
	}

	err = d.Pool.QueryRow(ctx, `
INSERT INTO users (display_name) VALUES ($1) RETURNING id
`, "labeller-"+uuid.NewString()[:8]).Scan(&labellerID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return samplePRID, labellerID
}

func insertLabel(t *testing.T, d *db.DB, samplePRID, labellerID uuid.UUID, verdict, reason, confidence string) (uuid.UUID, error) {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO calibration_labels (sample_pr_id, labeller_id, verdict, reason, confidence)
VALUES ($1, $2, $3, $4, $5) RETURNING id
`, samplePRID, labellerID, verdict, reason, confidence).Scan(&id)
	return id, err
}

// TestLabels_AreAppendOnly is the enforcement, not the convention.
//
// A label that can be updated is a label whose history can be rewritten after a
// calibration number has been quoted from it, and a label that can be deleted
// is one whose change leaves no evidence. Both are refused by the database, so
// no handler, migration or psql session can do it by accident - including one
// written by someone who never read the design.
func TestLabels_AreAppendOnly(t *testing.T) {
	d := dbtest.DB(t)
	samplePRID, labellerID := seedLabelRow(t, d)
	ctx := context.Background()

	id, err := insertLabel(t, d, samplePRID, labellerID, "accept", "meets the acceptance criteria in the linked issue", "certain")
	if err != nil {
		t.Fatalf("insert label: %v", err)
	}

	_, err = d.Pool.Exec(ctx, `UPDATE calibration_labels SET verdict = 'reject' WHERE id = $1`, id)
	if err == nil {
		t.Error("UPDATE succeeded; labels must be append-only")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE failed for the wrong reason: %v", err)
	}

	_, err = d.Pool.Exec(ctx, `DELETE FROM calibration_labels WHERE id = $1`, id)
	if err == nil {
		t.Error("DELETE succeeded; labels must be append-only")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE failed for the wrong reason: %v", err)
	}

	// The original survived both attempts, unchanged.
	var verdict string
	if err := d.Pool.QueryRow(ctx, `SELECT verdict FROM calibration_labels WHERE id = $1`, id).Scan(&verdict); err != nil {
		t.Fatalf("original row is gone: %v", err)
	}
	if verdict != "accept" {
		t.Errorf("verdict = %q, want accept", verdict)
	}
}

// TestLabels_AChangedMindIsANewRow is the other half: append-only must not mean
// "a labeller is stuck with their first answer". Changing a label is supported,
// as a new row pointing at the one it supersedes.
func TestLabels_AChangedMindIsANewRow(t *testing.T) {
	d := dbtest.DB(t)
	samplePRID, labellerID := seedLabelRow(t, d)
	ctx := context.Background()

	first, err := insertLabel(t, d, samplePRID, labellerID, "accept", "looked complete on a first read", "borderline")
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}

	var second uuid.UUID
	err = d.Pool.QueryRow(ctx, `
INSERT INTO calibration_labels (sample_pr_id, labeller_id, verdict, reason, confidence, supersedes_id)
VALUES ($1, $2, 'reject', 'on re-reading, the tests do not cover the criterion in the issue', 'certain', $3)
RETURNING id
`, samplePRID, labellerID, first).Scan(&second)
	if err != nil {
		t.Fatalf("insert superseding label: %v", err)
	}

	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM calibration_labels WHERE sample_pr_id = $1`, samplePRID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("rows = %d, want 2: the original must survive alongside the correction", count)
	}
}

// TestLabels_RequireASubstantiveReason. A verdict with no reason is unusable for
// calibration: when the model disagrees with a human, the reason is the only
// thing that says which of them was wrong.
func TestLabels_RequireASubstantiveReason(t *testing.T) {
	d := dbtest.DB(t)
	samplePRID, labellerID := seedLabelRow(t, d)

	for _, reason := range []string{"", "   ", "no", "too short"} {
		if _, err := insertLabel(t, d, samplePRID, labellerID, "accept", reason, "certain"); err == nil {
			t.Errorf("reason %q was accepted; it should not be", reason)
		}
	}
	if _, err := insertLabel(t, d, samplePRID, labellerID, "accept", "the diff implements what the issue asked for", "certain"); err != nil {
		t.Errorf("a substantive reason was rejected: %v", err)
	}
}

// TestLabels_RejectUnknownVerdictAndConfidence keeps the vocabulary closed. A
// stray value here would silently split the agreement calculation.
func TestLabels_RejectUnknownVerdictAndConfidence(t *testing.T) {
	d := dbtest.DB(t)
	samplePRID, labellerID := seedLabelRow(t, d)

	if _, err := insertLabel(t, d, samplePRID, labellerID, "maybe", "a perfectly good reason string", "certain"); err == nil {
		t.Error("verdict 'maybe' was accepted")
	}
	if _, err := insertLabel(t, d, samplePRID, labellerID, "accept", "a perfectly good reason string", "fairly sure"); err == nil {
		t.Error("confidence 'fairly sure' was accepted")
	}
}
