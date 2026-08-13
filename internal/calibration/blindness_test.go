package calibration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/calibration"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// Two sentinels, chosen so that a match cannot be a coincidence and a
// substring search is conclusive.
const (
	aiSentinel    = "AI-VERDICT-SENTINEL-b4e1c7"
	otherSentinel = "OTHER-LABELLER-REASON-SENTINEL-9f2a13"
)

type blindFixture struct {
	d          *db.DB
	sampleName string
	samplePRID uuid.UUID
	alice      calibration.Labeller
	bob        calibration.Labeller
}

func setupBlind(t *testing.T) blindFixture {
	t.Helper()
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ($1) RETURNING id`, "own-"+suffix).Scan(&ownerID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	var projectID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (github_full_name, github_repo_id, owner_user_id) VALUES ($1,$2,$3) RETURNING id
`, "blind/"+suffix, int64(uuid.New().ID()), ownerID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	prID := uuid.New()

	sampleName := "blind-" + suffix
	var sampleID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_samples (name, seed, candidate_pr_ids, candidate_hash, strata)
VALUES ($1, 1, $2, 'hash', '{}'::jsonb) RETURNING id
`, sampleName, []uuid.UUID{prID}).Scan(&sampleID); err != nil {
		t.Fatalf("sample: %v", err)
	}

	var samplePRID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_sample_prs (sample_id, pull_request_id, pr_number, project_full_name, merged, size_band)
VALUES ($1,$2,7,'blind/repo',true,'small') RETURNING id
`, sampleID, prID).Scan(&samplePRID); err != nil {
		t.Fatalf("sample pr: %v", err)
	}

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO calibration_pr_snapshots
 (sample_pr_id, title, body, author_login, url, additions, deletions, changed_files, diff, files, issue_number, issue_title, issue_body)
VALUES ($1,'A title','A body','someone','https://example.test',10,2,1,'--- a.go\n+ line','[]'::jsonb, 42, 'Issue title', 'Issue body')
`, samplePRID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	mk := func(handle string) calibration.Labeller {
		var id uuid.UUID
		if err := d.Pool.QueryRow(ctx, `
INSERT INTO calibration_labellers (handle, display_name) VALUES ($1,$2) RETURNING id
`, handle+"-"+suffix, handle).Scan(&id); err != nil {
			t.Fatalf("labeller: %v", err)
		}
		return calibration.Labeller{ID: id, Handle: handle + "-" + suffix, DisplayName: handle}
	}

	return blindFixture{d: d, sampleName: sampleName, samplePRID: samplePRID, alice: mk("alice"), bob: mk("bob")}
}

// TestNoAIOutputReachesALabeller is the sentinel test.
//
// A verdict row is seeded with model output whose text cannot occur by
// accident, and the labelling response is serialised and searched **as bytes**.
// Asserting "the field is absent" would pass for a nested struct, an embedded
// map, a debug field, or a json:"-" somebody removes; asserting the bytes do
// not contain the sentinel would fail for all of them.
func TestNoAIOutputReachesALabeller(t *testing.T) {
	f := setupBlind(t)
	ctx := context.Background()

	// A judged verdict sitting in the product tables, as one would in a real
	// database once judging has run.
	var hackID, projID, ownerID uuid.UUID
	if err := f.d.Pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ('vo') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx, `
INSERT INTO projects (github_full_name, github_repo_id, owner_user_id) VALUES ($1,$2,$3) RETURNING id
`, "verdict/"+uuid.NewString()[:8], int64(uuid.New().ID()), ownerID).Scan(&projID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name) VALUES ('h') RETURNING id`).Scan(&hackID); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	if _, err := f.d.Pool.Exec(ctx, `
INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, github_login, judge_bucket, judge_payload)
VALUES ($1,$2,7,'someone','exceptional',$3::jsonb)
`, hackID, projID, `{"reasoning":"`+aiSentinel+`"}`); err != nil {
		t.Fatalf("verdict: %v", err)
	}

	pr, err := calibration.GetPRForLabelling(ctx, f.d.Pool, f.sampleName, f.samplePRID, f.alice.ID)
	if err != nil {
		t.Fatalf("GetPRForLabelling: %v", err)
	}
	body, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), aiSentinel) {
		t.Fatal("model output reached the labelling response; the whole labelled set would be void")
	}
	for _, banned := range []string{"exceptional", "judge_bucket", "judge_payload", "cross_check", "escalation"} {
		if strings.Contains(string(body), banned) {
			t.Errorf("response mentions %q", banned)
		}
	}

	// The queue is rendered before a decision and must not prime one either.
	q, err := calibration.Queue(ctx, f.d.Pool, f.sampleName, f.alice.ID)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	qb, _ := json.Marshal(q)
	if strings.Contains(string(qb), aiSentinel) {
		t.Error("model output reached the queue response")
	}
}

// TestALabellerCannotSeeAnothersVerdictBeforeSubmitting is the same test for
// the second rule, and it matters for the same reason.
//
// If Alice labels first and Bob sees her verdict while deciding, the agreement
// rate measures influence rather than agreement. Enforced in SQL and asserted
// on bytes.
func TestALabellerCannotSeeAnothersVerdictBeforeSubmitting(t *testing.T) {
	f := setupBlind(t)
	ctx := context.Background()

	if _, err := calibration.SubmitLabel(ctx, f.d.Pool, f.samplePRID, f.alice.ID,
		"accept", otherSentinel+" - it meets the criteria", "certain"); err != nil {
		t.Fatalf("alice submits: %v", err)
	}

	// Bob has not labelled. Nothing he can reach may carry Alice's work.
	pr, err := calibration.GetPRForLabelling(ctx, f.d.Pool, f.sampleName, f.samplePRID, f.bob.ID)
	if err != nil {
		t.Fatalf("bob detail: %v", err)
	}
	prBytes, _ := json.Marshal(pr)
	if strings.Contains(string(prBytes), otherSentinel) {
		t.Fatal("Bob's PR detail carried Alice's reason before he submitted")
	}
	if pr.MyLabel != nil {
		t.Error("Bob was shown a label as his own before submitting one")
	}

	q, err := calibration.Queue(ctx, f.d.Pool, f.sampleName, f.bob.ID)
	if err != nil {
		t.Fatalf("bob queue: %v", err)
	}
	qBytes, _ := json.Marshal(q)
	if strings.Contains(string(qBytes), otherSentinel) {
		t.Fatal("Bob's queue carried Alice's reason")
	}
	for _, item := range q {
		if item.SamplePRID == f.samplePRID && item.Labelled {
			t.Error("Bob's queue marked a pull request as labelled on the strength of Alice's label")
		}
	}

	// Agreement is not computable for Bob yet either - that endpoint exists to
	// show other people's answers, so it is the one that most needs the rule.
	rowsBefore, err := calibration.Agreement(ctx, f.d.Pool, f.sampleName, f.bob.ID)
	if err != nil {
		t.Fatalf("agreement before: %v", err)
	}
	ab, _ := json.Marshal(rowsBefore)
	if strings.Contains(string(ab), otherSentinel) || len(rowsBefore) != 0 {
		t.Fatalf("agreement leaked before Bob submitted: %s", ab)
	}

	// After Bob submits, comparison becomes available - which is the point.
	if _, err := calibration.SubmitLabel(ctx, f.d.Pool, f.samplePRID, f.bob.ID,
		"reject", "the tests do not cover the criterion", "borderline"); err != nil {
		t.Fatalf("bob submits: %v", err)
	}
	rowsAfter, err := calibration.Agreement(ctx, f.d.Pool, f.sampleName, f.bob.ID)
	if err != nil {
		t.Fatalf("agreement after: %v", err)
	}
	if len(rowsAfter) != 1 {
		t.Fatalf("agreement rows = %d, want 1 once both have submitted", len(rowsAfter))
	}
	if rowsAfter[0].Agree {
		t.Error("accept vs reject was reported as agreement")
	}
	if len(rowsAfter[0].Verdicts) != 2 {
		t.Errorf("verdicts = %v, want both labellers", rowsAfter[0].Verdicts)
	}
}

// A returning labeller sees their own previous verdict - that is not a leak,
// and losing it would mean nobody could review or revise their own work.
func TestALabellerSeesTheirOwnPreviousLabel(t *testing.T) {
	f := setupBlind(t)
	ctx := context.Background()

	if _, err := calibration.SubmitLabel(ctx, f.d.Pool, f.samplePRID, f.alice.ID,
		"accept", "a reason long enough to pass the check", "certain"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	pr, err := calibration.GetPRForLabelling(ctx, f.d.Pool, f.sampleName, f.samplePRID, f.alice.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if pr.MyLabel == nil || pr.MyLabel.Verdict != "accept" {
		t.Fatalf("own label missing: %+v", pr.MyLabel)
	}
}

// Held-back pull requests are not reachable at all - not in the queue, and not
// by asking for one directly.
func TestHeldBackIsUnreachable(t *testing.T) {
	f := setupBlind(t)
	ctx := context.Background()

	if _, err := f.d.Pool.Exec(ctx, `UPDATE calibration_sample_prs SET held_back = true WHERE id = $1`, f.samplePRID); err != nil {
		t.Fatalf("hold back: %v", err)
	}

	q, err := calibration.Queue(ctx, f.d.Pool, f.sampleName, f.alice.ID)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	for _, item := range q {
		if item.SamplePRID == f.samplePRID {
			t.Error("a held-back pull request appeared in the queue")
		}
	}
	if _, err := calibration.GetPRForLabelling(ctx, f.d.Pool, f.sampleName, f.samplePRID, f.alice.ID); err == nil {
		t.Error("a held-back pull request was served directly by id")
	}
}

// The "no linked issue" case must be distinguishable from a failure to load.
func TestNoLinkedIssueIsExplicit(t *testing.T) {
	f := setupBlind(t)
	ctx := context.Background()

	if _, err := f.d.Pool.Exec(ctx, `
UPDATE calibration_pr_snapshots SET issue_number = NULL, issue_title = NULL, issue_body = NULL
WHERE sample_pr_id = $1`, f.samplePRID); err != nil {
		t.Fatalf("clear issue: %v", err)
	}

	pr, err := calibration.GetPRForLabelling(ctx, f.d.Pool, f.sampleName, f.samplePRID, f.alice.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if pr.HasIssue {
		t.Error("HasIssue is true with no issue stored")
	}
	if pr.IssueTitle != "" || pr.IssueBody != "" {
		t.Error("issue fields are non-empty; the screen could render a blank panel that looks like a loading failure")
	}
}
