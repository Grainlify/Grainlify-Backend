package calibration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/calibration"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestScoreRun_IsUnchangedByALaterCorrection is the as-of guarantee.
//
// Labels are append-only and a labeller may correct themselves later - one
// did, after a model disagreed and turned out to be right. If scoring read the
// latest label, that correction would silently restate every historical run:
// a number quoted last week would not be the number the same command printed
// today, and nobody would see it move.
//
// A run is therefore scored against the label as it stood when the run
// happened. This test adds a superseding label AFTER a recorded run and
// asserts the run's score, its baseline and its disagreement list are all
// exactly as they were.
//
// **This property is one innocent-looking query change away from being gone.**
// Dropping the `created_at <= r.started_at` predicate, or replacing the
// LATERAL with a simple join on the newest label, restores the old behaviour
// silently - the code still compiles, every other test still passes, and the
// numbers only diverge once somebody corrects a label. That is why this test
// exists and why it asserts the whole shape rather than one number.
func TestScoreRun_IsUnchangedByALaterCorrection(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	// A sample with two rows: one the model agrees with, one it does not.
	var sampleID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_samples (name, seed, candidate_pr_ids, candidate_hash, strata)
VALUES ($1, 1, ARRAY[]::uuid[], 'h', '{}'::jsonb) RETURNING id`, "asof-"+suffix).Scan(&sampleID); err != nil {
		t.Fatalf("sample: %v", err)
	}

	var labellerID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_labellers (handle, display_name) VALUES ($1,'A') RETURNING id`,
		"asof-"+suffix).Scan(&labellerID); err != nil {
		t.Fatalf("labeller: %v", err)
	}

	newRow := func(number int, humanVerdict string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_sample_prs (sample_id, pull_request_id, pr_number, project_full_name, merged, size_band)
VALUES ($1,$2,$3,'asof/repo',true,'small') RETURNING id`, sampleID, uuid.New(), number).Scan(&id); err != nil {
			t.Fatalf("sample pr: %v", err)
		}
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_pr_snapshots
 (sample_pr_id, title, additions, deletions, changed_files, diff, files)
VALUES ($1,'t',1,0,1,'d','[]'::jsonb)`, id); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		// Labelled an hour ago, comfortably before the run.
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_labels (sample_pr_id, labeller_id, verdict, reason, confidence, created_at)
VALUES ($1,$2,$3,'a reason long enough to pass','certain', now() - interval '1 hour')`,
			id, labellerID, humanVerdict); err != nil {
			t.Fatalf("label: %v", err)
		}
		return id
	}

	agreed := newRow(1, "accept")
	corrected := newRow(2, "accept")

	// A run half an hour ago: the model accepted one and rejected the other,
	// so as things stood then, agreement was 1 of 2.
	var runID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_model_runs
 (sample_id, provider, model, prompt_version, prompt_sha256, long_context_threshold,
  input_price_per_mtok, output_price_per_mtok, scope, started_at)
VALUES ($1,'openai','m','v','sha',128000,1,1,'main', now() - interval '30 minutes') RETURNING id`,
		sampleID).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}
	for id, verdict := range map[uuid.UUID]string{agreed: "accept", corrected: "reject"} {
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_model_verdicts (run_id, sample_pr_id, verdict, reason, prompt_tokens, completion_tokens)
VALUES ($1,$2,$3,'because',10,5)`, runID, id, verdict); err != nil {
			t.Fatalf("model verdict: %v", err)
		}
	}

	before, err := calibration.ScoreRun(ctx, d.Pool, runID)
	if err != nil {
		t.Fatalf("score before: %v", err)
	}
	if before.Agree != 1 || before.Scored != 2 {
		t.Fatalf("before: agree %d of %d, want 1 of 2", before.Agree, before.Scored)
	}
	if before.BaselineAccepts != 2 || before.BaselineTotal != 2 {
		t.Fatalf("before: baseline %d of %d, want 2 of 2", before.BaselineAccepts, before.BaselineTotal)
	}

	// The labeller now agrees the model was right, and corrects themselves.
	// Append-only: a new row pointing at the one it supersedes.
	var priorID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
SELECT id FROM calibration_labels WHERE sample_pr_id = $1 ORDER BY created_at DESC LIMIT 1`,
		corrected).Scan(&priorID); err != nil {
		t.Fatalf("prior: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_labels (sample_pr_id, labeller_id, verdict, reason, confidence, supersedes_id)
VALUES ($1,$2,'reject','on re-reading the model was right about this one','certain',$3)`,
		corrected, labellerID, priorID); err != nil {
		t.Fatalf("correction: %v", err)
	}

	after, err := calibration.ScoreRun(ctx, d.Pool, runID)
	if err != nil {
		t.Fatalf("score after: %v", err)
	}

	if after.Agree != before.Agree || after.Scored != before.Scored {
		t.Errorf("a later correction restated a recorded run: agreement went %d/%d -> %d/%d",
			before.Agree, before.Scored, after.Agree, after.Scored)
	}
	if after.AgreementPct() != before.AgreementPct() {
		t.Errorf("agreement percentage moved: %.1f -> %.1f", before.AgreementPct(), after.AgreementPct())
	}
	// The baseline is the half that moved first when this was wrong, so it is
	// asserted explicitly rather than assumed to follow.
	if after.BaselinePct != before.BaselinePct || after.BaselineAccepts != before.BaselineAccepts {
		t.Errorf("baseline moved under a correction: %.0f%% (%d) -> %.0f%% (%d)",
			before.BaselinePct, before.BaselineAccepts, after.BaselinePct, after.BaselineAccepts)
	}
	if len(after.Disagreements) != len(before.Disagreements) {
		t.Errorf("disagreement list changed: %d -> %d", len(before.Disagreements), len(after.Disagreements))
	}

	// And a run started AFTER the correction must see it - a frozen past is
	// only correct if the present still moves.
	var laterRun uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_model_runs
 (sample_id, provider, model, prompt_version, prompt_sha256, long_context_threshold,
  input_price_per_mtok, output_price_per_mtok, scope)
VALUES ($1,'openai','m','v','sha',128000,1,1,'main') RETURNING id`, sampleID).Scan(&laterRun); err != nil {
		t.Fatalf("later run: %v", err)
	}
	for id, verdict := range map[uuid.UUID]string{agreed: "accept", corrected: "reject"} {
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_model_verdicts (run_id, sample_pr_id, verdict, reason)
VALUES ($1,$2,$3,'because')`, laterRun, id, verdict); err != nil {
			t.Fatalf("later verdict: %v", err)
		}
	}
	later, err := calibration.ScoreRun(ctx, d.Pool, laterRun)
	if err != nil {
		t.Fatalf("score later: %v", err)
	}
	if later.Agree != 2 {
		t.Errorf("a run after the correction agreed %d of %d, want 2 - corrections must apply going forward",
			later.Agree, later.Scored)
	}
}

// TestRunScopeIsRequired. Scope was added nullable, and a nullable column is
// how the guess comes back: some future path forgets it, writes NULL, and
// re-reporting cannot tell a main run from a held-back one. It already printed
// a 7-row run's figures when a 21-row run was asked for.
func TestRunScopeIsRequired(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var sampleID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_samples (name, seed, candidate_pr_ids, candidate_hash, strata)
VALUES ($1, 1, ARRAY[]::uuid[], 'h', '{}'::jsonb) RETURNING id`,
		"scope-"+uuid.NewString()[:8]).Scan(&sampleID); err != nil {
		t.Fatalf("sample: %v", err)
	}

	_, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_model_runs
 (sample_id, provider, model, prompt_version, prompt_sha256, long_context_threshold,
  input_price_per_mtok, output_price_per_mtok)
VALUES ($1,'openai','m','v','sha',128000,1,1)`, sampleID)
	if err == nil {
		t.Error("a run with no scope was accepted; -report will guess which run it is looking at")
	}

	_, err = d.Pool.Exec(ctx, `
INSERT INTO calibration_model_runs
 (sample_id, provider, model, prompt_version, prompt_sha256, long_context_threshold,
  input_price_per_mtok, output_price_per_mtok, scope)
VALUES ($1,'openai','m','v','sha',128000,1,1,'everything')`, sampleID)
	if err == nil {
		t.Error("an unknown scope was accepted; a run nobody can categorise is a run nobody can find")
	}
}
