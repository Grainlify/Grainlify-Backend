package hackathon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// jxFxMergedPR inserts a merged PR whose body closes the given issue.
func jxFxMergedPR(t *testing.T, pool db.DBPool, projectID uuid.UUID, prNumber, issueNumber int, author string, mergedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests
  (project_id, github_pr_id, number, state, title, body, author_login, url, merged, merged_at_github, merge_commit_sha)
VALUES ($1,$2,$3,'closed','PR',$4,$5,'https://example.test',true,$6,$7)
`, projectID, int64(prNumber)*100000+int64(uuid.New().ID()%1000), prNumber,
		fmt.Sprintf("Fixes #%d", issueNumber), author, mergedAt,
		fmt.Sprintf("sha-%d", prNumber)); err != nil {
		t.Fatalf("jxFxMergedPR: %v", err)
	}
}

func jxFxGitHubIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, number int, authorLogin string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login)
VALUES ($1,$2,$3,'closed','Issue',$4)
ON CONFLICT DO NOTHING
`, projectID, int64(number)*77777+int64(uuid.New().ID()%1000), number, authorLogin); err != nil {
		t.Fatalf("jxFxGitHubIssue: %v", err)
	}
}

func jxVerdict(t *testing.T, pool db.DBPool, projectID uuid.UUID, prNumber int) (status, reason string) {
	t.Helper()
	var r *string
	if err := pool.QueryRow(context.Background(), `
SELECT prefilter_status, prefilter_reason FROM hackathon_verdicts
WHERE project_id = $1 AND pr_number = $2`, projectID, prNumber).Scan(&status, &r); err != nil {
		t.Fatalf("jxVerdict(%d): %v", prNumber, err)
	}
	if r != nil {
		reason = *r
	}
	return status, reason
}

// gh is nil throughout: these cover the §2.4 conditions decided from our own
// data, which is exactly the set that must be answered without GitHub.
func TestSyncVerdicts_RecordsWhyAPRDidNotQualify(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 500, "standard")
	jxFxGitHubIssue(t, pool, projectID, 500, "maintainer-alice")

	assignee := fxUser(t, pool)
	fxGitHubAccount(t, pool, assignee, "assigned-bob")
	fxAssignment(t, pool, hackathonID, issueID, projectID, assignee, 500, "assigned-bob", nil)

	// An outsider fixes the issue in good faith and gets it merged. This is
	// the case that most needs an explanation rather than silence.
	jxFxMergedPR(t, pool, projectID, 900, 500, "helpful-outsider", time.Now())

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("SyncVerdicts: %v", err)
	}

	status, reason := jxVerdict(t, pool, projectID, 900)
	if status != "rejected" {
		t.Errorf("status = %q, want rejected", status)
	}
	if reason == "" {
		t.Fatal("no reason recorded - the absence of an explanation is what this exists to prevent")
	}
	// It must name who it *was* assigned to, so the outsider understands
	// the rule rather than just the outcome.
	if !strings.Contains(reason, "assigned-bob") {
		t.Errorf("reason = %q, want it to name the assigned contributor", reason)
	}
}

func TestSyncVerdicts_PassesTheAssignedContributor(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 501, "standard")
	jxFxGitHubIssue(t, pool, projectID, 501, "maintainer-alice")

	assignee := fxUser(t, pool)
	fxGitHubAccount(t, pool, assignee, "assigned-bob")
	fxAssignment(t, pool, hackathonID, issueID, projectID, assignee, 501, "assigned-bob", nil)
	jxFxMergedPR(t, pool, projectID, 901, 501, "Assigned-Bob", time.Now()) // case-insensitive

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("SyncVerdicts: %v", err)
	}
	// gh is nil, so diff stats can't be fetched and the row stays pending
	// rather than being judged on nothing.
	status, reason := jxVerdict(t, pool, projectID, 901)
	if status != "pending" {
		t.Errorf("status = %q (reason %q), want pending - it passed §2.4 but has no diff yet", status, reason)
	}
}

func TestSyncVerdicts_Section24Conditions(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	// Pin the window so the merge-time cases are deterministic.
	//
	// ends_at is three days back, not two, and the third day is the whole
	// point. With a 48h grace, `ends_at = now() - 2 days` put the deadline at
	// EXACTLY the Postgres now() of this statement - and the "merged well past
	// the grace period" case below is then seeded with Go's time.Now(), a few
	// hundred milliseconds later on a DIFFERENT CLOCK.
	//
	// The assertion's entire margin was therefore "how long the seeding took,
	// minus however far the Postgres container's clock runs ahead of the
	// host's". Measured at 20-90ms on Docker Desktop, which is enough to lose:
	// the test failed 4 runs in 5 locally while passing on CI, where host and
	// container share a kernel clock. A test that fails on developer machines
	// and passes in CI is worse than one that fails everywhere, because the
	// green tick is the least informative result available.
	//
	// Three days moves the deadline to now() - 1 day, so the post-grace PR
	// clears it by roughly 24 hours instead of by scheduler noise. The other
	// three cases are unaffected: the in-grace PR at now-36h is still inside
	// the new deadline and still after starts_at, and the pre-start and
	// self-authored cases do not depend on ends_at at all.
	//
	// The deeper fix is for this fixture to take every timestamp from ONE
	// clock - read now() from Postgres once and derive the seeds from it, or
	// write merged_at_github in SQL. Worth doing if this grows more
	// time-sensitive cases; not done here because a one-line change with 24
	// hours of margin is verifiable in a way a refactor of four fixtures is
	// not.
	if _, err := pool.Exec(ctx, `
UPDATE hackathons SET starts_at = now() - interval '10 days', ends_at = now() - interval '3 days',
  merge_grace_period_hours = 48 WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set window: %v", err)
	}

	seed := func(issueNumber, prNumber int, author string, mergedAt time.Time, issueAuthor string) {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, issueNumber, "standard")
		jxFxGitHubIssue(t, pool, projectID, issueNumber, issueAuthor)
		u := fxUser(t, pool)
		fxGitHubAccount(t, pool, u, "assigned-bob")
		fxAssignment(t, pool, hackathonID, issueID, projectID, u, issueNumber, "assigned-bob", nil)
		jxFxMergedPR(t, pool, projectID, prNumber, issueNumber, author, mergedAt)
	}

	// Merged before the event started.
	seed(510, 910, "assigned-bob", time.Now().Add(-20*24*time.Hour), "maintainer-alice")
	// Merged inside the grace period after the end - this one qualifies.
	seed(511, 911, "assigned-bob", time.Now().Add(-36*time.Hour), "maintainer-alice")
	// Merged well past the grace period.
	seed(512, 912, "assigned-bob", time.Now(), "maintainer-alice")
	// PR author opened the issue.
	seed(513, 913, "assigned-bob", time.Now().Add(-36*time.Hour), "assigned-bob")

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("SyncVerdicts: %v", err)
	}

	if s, r := jxVerdict(t, pool, projectID, 910); s != "rejected" || !strings.Contains(r, "before this GrainHack started") {
		t.Errorf("pre-start PR: (%s, %q)", s, r)
	}
	// The grace period exists so maintainer review latency doesn't cost the
	// contributor; a merge 36h after close is inside 48h.
	if s, _ := jxVerdict(t, pool, projectID, 911); s != "pending" {
		t.Errorf("in-grace PR status = %q, want pending (it qualified)", s)
	}
	if s, r := jxVerdict(t, pool, projectID, 912); s != "rejected" || !strings.Contains(r, "grace period") {
		t.Errorf("post-grace PR: (%s, %q)", s, r)
	}
	if s, r := jxVerdict(t, pool, projectID, 913); s != "rejected" || !strings.Contains(r, "opened the issue") {
		t.Errorf("self-authored PR: (%s, %q)", s, r)
	}
}

// A PR that names no GrainHack issue was never in the event, so it gets no
// row at all - the alternative is a verdict row for every merged PR on the
// repo, which is noise, not an explanation.
func TestSyncVerdicts_IgnoresPRsThatNameNoGrainHackIssue(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxPublishedIssue(t, pool, hackathonID, projectID, 520, "standard")

	// Closes an issue that isn't in the hackathon.
	jxFxMergedPR(t, pool, projectID, 920, 999, "someone", time.Now())

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("SyncVerdicts: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_verdicts WHERE project_id = $1`, projectID); n != 0 {
		t.Errorf("verdict rows = %d, want 0", n)
	}
}

// Re-running the sync must not undo a human decision.
func TestSyncVerdicts_NeverOverwritesAHumanOverride(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 530, "standard")
	jxFxGitHubIssue(t, pool, projectID, 530, "maintainer-alice")
	u := fxUser(t, pool)
	fxGitHubAccount(t, pool, u, "assigned-bob")
	fxAssignment(t, pool, hackathonID, issueID, projectID, u, 530, "assigned-bob", nil)
	// An unassigned author, so a fresh sync would mark it rejected.
	jxFxMergedPR(t, pool, projectID, 930, 530, "outsider", time.Now())

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	admin := fxAdmin(t, pool)
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_verdicts
SET final_bucket = 'accepted', final_source = 'human_override',
    override_reason = 'Maintainer confirmed they asked this person to take it over.',
    overridden_by = $2, overridden_at = now(), prefilter_status = 'passed', prefilter_reason = NULL
WHERE project_id = $1 AND pr_number = 930`, projectID, admin); err != nil {
		t.Fatalf("override: %v", err)
	}

	if err := SyncVerdicts(ctx, pool, nil, "", projectID, "acme/widgets"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	status, reason := jxVerdict(t, pool, projectID, 930)
	if status != "passed" || reason != "" {
		t.Errorf("a re-sync reverted a human override: (%s, %q)", status, reason)
	}
	var finalBucket string
	if err := pool.QueryRow(ctx,
		`SELECT final_bucket FROM hackathon_verdicts WHERE project_id = $1 AND pr_number = 930`, projectID).Scan(&finalBucket); err != nil {
		t.Fatalf("read final bucket: %v", err)
	}
	if finalBucket != "accepted" {
		t.Errorf("final_bucket = %q, want the overridden accepted", finalBucket)
	}
}

// TestSyncOneVerdict_RepoAdminCondition covers §2.4 condition 6's repo-admin
// half - the collusion path §2.3 exists to close: a maintainer's second
// account is assigned legitimately through the platform, submits, and would
// otherwise be paid.
func TestSyncOneVerdict_RepoAdminCondition(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 540, "standard")
	jxFxGitHubIssue(t, pool, projectID, 540, "someone-else")

	u := fxUser(t, pool)
	fxGitHubAccount(t, pool, u, "maintainer-alt")
	fxAssignment(t, pool, hackathonID, issueID, projectID, u, 540, "maintainer-alt", nil)
	jxFxMergedPR(t, pool, projectID, 940, 540, "maintainer-alt", time.Now())

	candidates, err := loadCandidatePRs(ctx, pool, projectID)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("loadCandidatePRs: %d candidates, err %v", len(candidates), err)
	}
	c := candidates[0]

	t.Run("an admin author is rejected even though they were assigned", func(t *testing.T) {
		admins := map[string]bool{"maintainer-alt": true}
		if err := syncOneVerdict(ctx, pool, nil, "", projectID, "acme/widgets", c, admins); err != nil {
			t.Fatalf("syncOneVerdict: %v", err)
		}
		status, reason := jxVerdict(t, pool, projectID, 940)
		if status != "rejected" {
			t.Errorf("status = %q, want rejected", status)
		}
		if !strings.Contains(reason, "admin permission") {
			t.Errorf("reason = %q, want it to name the admin condition", reason)
		}
	})

	t.Run("a non-admin author is not rejected by this condition", func(t *testing.T) {
		admins := map[string]bool{"someone-entirely-different": true}
		if err := syncOneVerdict(ctx, pool, nil, "", projectID, "acme/widgets", c, admins); err != nil {
			t.Fatalf("syncOneVerdict: %v", err)
		}
		status, _ := jxVerdict(t, pool, projectID, 940)
		if status != "pending" {
			t.Errorf("status = %q, want pending (it passed §2.4; no diff fetched yet)", status)
		}
	})

	// The direction that matters: not knowing must never read as "not an
	// admin", or an unreadable permission list quietly re-opens the hole.
	t.Run("an unreadable admin list leaves the PR unjudged rather than passed", func(t *testing.T) {
		if err := syncOneVerdict(ctx, pool, nil, "", projectID, "acme/widgets", c, nil); err != nil {
			t.Fatalf("syncOneVerdict: %v", err)
		}
		status, _ := jxVerdict(t, pool, projectID, 940)
		if status == "passed" {
			t.Error("a PR was marked passed while the admin condition could not be checked")
		}
	})
}
