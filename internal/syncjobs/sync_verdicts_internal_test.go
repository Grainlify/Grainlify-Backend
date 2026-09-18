package syncjobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// The PR's patch is a real-sized change: twelve meaningful lines, above the
// event's default minimum of ten, so the prefilter has no reason to reject it.
//
// fakeGitHub answers the four calls a PR sync and its judging intake make,
// with GitHub's own response shapes. Anything else is a 404, so an
// unexpected call shows up as a missing verdict rather than passing quietly.
type fakeGitHub struct {
	repo   string
	prJSON string
	calls  []string
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	f.calls = append(f.calls, path+"?"+r.URL.RawQuery)
	body, code := `{"message":"Not Found"}`, http.StatusNotFound
	switch {
	case path == "/repos/"+f.repo+"/pulls" && r.URL.Query().Get("page") == "1":
		body, code = "["+f.prJSON+"]", http.StatusOK
	case path == "/repos/"+f.repo+"/pulls":
		body, code = "[]", http.StatusOK // every page after the last is empty - this is what ended the loop early
	case path == "/repos/"+f.repo+"/collaborators":
		body, code = "[]", http.StatusOK
	case strings.HasPrefix(path, "/repos/"+f.repo+"/pulls/") && strings.HasSuffix(path, "/files"):
		body, code = `[{"filename":"worker/retry.go","status":"modified","additions":12,"deletions":0,"changes":12,"patch":"@@ -45,2 +45,14 @@\n+delay := p.BaseDelay\n+if attempt == p.MaxAttempts {\n+\tbreak\n+}\n+if p.MaxDelay > 0 && delay > p.MaxDelay {\n+\tdelay = p.MaxDelay\n+}\n+if serr := sleep(ctx, delay); serr != nil {\n+\treturn fmt.Errorf(\"stopped retrying: %w\", serr)\n+}\n+delay *= 2\n+log.Printf(\"retrying in %v\", delay)"}]`, http.StatusOK
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// A merged PR that closes a GrainHack issue must become a verdict when the
// project's PRs are synced. It never did: syncPRs returned on the first empty
// page, before the judging intake at the end of the function.
func TestSyncPRs_TurnsAMergedGrainHackPRIntoAVerdict(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	newUser := func() uuid.UUID {
		var id uuid.UUID
		if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	ownerID, contributorID := newUser(), newUser()
	fullName := "octo/verdict-sync-" + uuid.NewString()[:8]
	login := "contrib-" + uuid.NewString()[:8]

	var projectID, hackathonID, issueID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	starts := time.Now().Add(-48 * time.Hour)
	ends := time.Now().Add(48 * time.Hour)
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name, phase, starts_at, ends_at) VALUES ($1, 'live', $2, $3) RETURNING id`,
		"verdict-sync-"+uuid.NewString(), starts, ends).Scan(&hackathonID); err != nil {
		t.Fatalf("insert hackathon: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, 'desc', 'goal', 1, 'contact@example.com', 'accepted')`, hackathonID, projectID, ownerID); err != nil {
		t.Fatalf("insert project application: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO hackathon_issues (hackathon_id, project_id, issue_number, org_login, status, difficulty_tier, acceptance_criteria, published_at)
VALUES ($1, $2, 1, 'octo', 'published', 'easy', 'Back off between retries', now()) RETURNING id`,
		hackathonID, projectID).Scan(&issueID); err != nil {
		t.Fatalf("insert hackathon issue: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_assignments (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status)
VALUES ($1, $2, $3, 1, $4, $5, 'octo', 'active')`, hackathonID, issueID, projectID, contributorID, login); err != nil {
		t.Fatalf("insert assignment: %v", err)
	}

	mergedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	gh := &fakeGitHub{repo: fullName, prJSON: fmt.Sprintf(`{
  "id": 990001, "number": 7, "state": "closed", "title": "Fix the flaky retry loop",
  "body": "Adds exponential backoff.\n\nFixes #1", "html_url": "https://github.com/%s/pull/7",
  "user": {"login": %q}, "merged_at": %q, "merge_commit_sha": "abc123",
  "head": {"sha": "def456"}, "created_at": %q, "updated_at": %q, "closed_at": %q
}`, fullName, login, mergedAt, mergedAt, mergedAt, mergedAt)}

	w := &Worker{
		pool:     d.Pool,
		limiter:  rate.NewLimiter(rate.Inf, 1),
		gh:       &github.Client{HTTP: &http.Client{Transport: gh}, UserAgent: "test"},
		workerID: "test",
	}
	if err := w.syncPRs(ctx, projectID, fullName, "test-token"); err != nil {
		t.Fatalf("syncPRs: %v (calls: %v)", err, gh.calls)
	}

	var merged bool
	if err := d.Pool.QueryRow(ctx, `SELECT merged FROM github_pull_requests WHERE project_id = $1 AND number = 7`, projectID).Scan(&merged); err != nil || !merged {
		t.Fatalf("PR row: merged=%v err=%v - the sync itself did not record the merge", merged, err)
	}
	var verdicts int
	var status string
	if err := d.Pool.QueryRow(ctx, `
SELECT count(*), COALESCE(max(prefilter_status), '') || ': ' || COALESCE(max(prefilter_reason), '') FROM hackathon_verdicts
WHERE hackathon_id = $1 AND project_id = $2 AND pr_number = 7`, hackathonID, projectID).Scan(&verdicts, &status); err != nil {
		t.Fatalf("count verdicts: %v", err)
	}
	if verdicts != 1 {
		t.Fatalf("verdict rows for the merged PR = %d, want 1: the judging intake never ran (GitHub calls: %v)", verdicts, gh.calls)
	}
	if strings.HasPrefix(status, "rejected") {
		t.Errorf("verdict prefilter_status = %q; this PR meets every qualification, so a rejection means the fixture or the intake is wrong", status)
	}
}
