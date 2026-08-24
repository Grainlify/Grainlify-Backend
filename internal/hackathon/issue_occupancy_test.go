package hackathon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The defect, on the success path: a merged PR must not re-advertise its own
// issue.
//
// This is the ordinary outcome of an event going well, not an edge case. The
// old draw selection excluded issues whose assignment was 'active' or
// 'pr_submitted'. RecordMerge moves it to 'completed', which matched neither,
// so the moment work was finished the issue read as free: it re-entered the due
// set, its applications had all been resolved to won/lost so the applicant
// count was zero, and the empty-window branch re-opened the window on finished
// work.
func TestRunDueDraws_DoesNotReAdvertiseAMergedIssue(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 700, "standard")
	fxApplicant(t, pool, hackathonID, issueID, "merge-a", "plausible")
	fxCloseWindow(t, pool, issueID)

	if err := r.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}
	var login string
	if err := pool.QueryRow(ctx, `
SELECT github_login FROM hackathon_assignments WHERE hackathon_issue_id = $1`, issueID).Scan(&login); err != nil {
		t.Fatalf("read the assignment the draw made: %v", err)
	}

	// The PR merges. This is the whole trigger.
	if err := RecordMerge(ctx, pool, hackathonID, projectID, 700, login); err != nil {
		t.Fatalf("RecordMerge: %v", err)
	}

	// The window is still closed in the past and the issue is still
	// 'published' - nothing about a merge changes either, which is what put
	// the issue back in front of the runner.
	fxCloseWindow(t, pool, issueID)

	for i := 0; i < 3; i++ {
		if err := r.runDueDraws(ctx); err != nil {
			t.Fatalf("runDueDraws tick %d: %v", i, err)
		}
	}

	// Two independent symptoms, because they fail for different reasons: a
	// re-opened window is the issue being advertised again, and a second
	// assignment is somebody actually being given finished work.
	var opensAt, closesAt *string
	if err := pool.QueryRow(ctx, `
SELECT application_window_opens_at::text, application_window_closes_at::text
FROM hackathon_issues WHERE id = $1`, issueID).Scan(&opensAt, &closesAt); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if n := countRows(t, pool, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_issue_id = $1 AND NOT hackathon_assignment_released(status)`, issueID); n != 1 {
		t.Errorf("unreleased assignments on a merged issue = %d, want 1 (the person who did the work)", n)
	}
	if n := countRows(t, pool, `
SELECT count(*) FROM hackathon_draws WHERE hackathon_issue_id = $1 AND NOT is_simulation`, issueID); n != 1 {
		t.Errorf("committed draws for a merged issue = %d, want 1; the issue was drawn again after its work was done", n)
	}
}

// ReopenWindow refuses finished work whoever calls it.
//
// Asserted at ReopenWindow rather than at a caller on purpose. It has three
// callers - the empty-window retry, the stale-release path, and a handler
// reachable over HTTP - and a guard in the one where the defect was found would
// have left the other two able to re-advertise a completed issue.
func TestReopenWindow_RefusesAnIssueWhoseWorkIsFinished(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 701, "standard")

	userID := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,701,$4,'finished','acme','completed',false)`,
		hackathonID, issueID, projectID, userID); err != nil {
		t.Fatalf("seed completed assignment: %v", err)
	}

	err := ReopenWindow(ctx, pool, hackathonID, issueID)
	if !errors.Is(err, ErrIssueFinished) {
		t.Fatalf("ReopenWindow on a completed issue = %v, want ErrIssueFinished", err)
	}
}

// An issue that was released IS re-drawable - the fix must not close that.
//
// The guard is "finished", not "has ever been assigned". Without this, the
// change would silently convert every abandoned assignment into a permanently
// dead issue, which is a worse failure than the one being fixed and would look
// like the retry logic breaking.
func TestReopenWindow_StillAllowsAReleasedIssue(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 702, "standard")

	userID := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,702,$4,'gaveup','acme','released_stale',false)`,
		hackathonID, issueID, projectID, userID); err != nil {
		t.Fatalf("seed released assignment: %v", err)
	}

	if err := ReopenWindow(ctx, pool, hackathonID, issueID); err != nil {
		t.Fatalf("ReopenWindow on a released issue: %v, want it to reopen", err)
	}
}

// The database enforces it too, not only the Go path.
//
// The predicate is one SQL function used by both the index and every query, so
// this also pins that the index actually carries the new rule: under the old
// predicate ('active','pr_submitted') this insert succeeded.
func TestUniqueIndex_ACompletedAssignmentBlocksASecondOne(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 703, "standard")

	first := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,703,$4,'did-the-work','acme','completed',false)`,
		hackathonID, issueID, projectID, first); err != nil {
		t.Fatalf("seed completed assignment: %v", err)
	}

	second := fxUser(t, pool)
	_, err := pool.Exec(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,703,$4,'second-winner','acme','active',true)`,
		hackathonID, issueID, projectID, second)
	if err == nil {
		t.Fatal("a second active assignment was accepted for an issue whose work is already completed")
	}
}

// IssueIsFinished distinguishes the three cases it has to.
func TestIssueIsFinished(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	seed := func(number int, status string) uuid.UUID {
		id := fxPublishedIssue(t, pool, hackathonID, projectID, number, "standard")
		if status != "" {
			if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status, holds_slot)
VALUES ($1,$2,$3,$4,$5,'someone','acme',$6,false)`,
				hackathonID, id, projectID, number, fxUser(t, pool), status); err != nil {
				t.Fatalf("seed %s: %v", status, err)
			}
		}
		return id
	}

	for _, tc := range []struct {
		name   string
		status string
		want   bool
	}{
		{"never assigned", "", false},
		{"active", "active", false},
		{"pr submitted", "pr_submitted", false},
		{"released", "released_stale", false},
		{"completed", "completed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seed(710+len(tc.name), tc.status)
			got, err := IssueIsFinished(context.Background(), pool, id)
			if err != nil {
				t.Fatalf("IssueIsFinished: %v", err)
			}
			if got != tc.want {
				t.Errorf("IssueIsFinished(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// #490: an event with no issues cannot go live.
//
// The failure it prevents is silence. Every component behaves correctly - the
// runner looks for issues with a closed window, finds none, and does nothing -
// so there is no error anywhere and the first person to notice is a
// contributor looking at an event with nothing to apply to.
func TestTransition_RefusesGoingLiveWithNoPublishedIssues(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	admin := fxAdmin(t, pool)
	ownerID := fxUser(t, pool)
	projectID := fxProject(t, pool, ownerID, "acme")
	hackathonID := fxHackathon(t, pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, pool, hackathonID, projectID, ownerID)
	fxSetConfig(t, pool, hackathonID, "judging_shadow_mode", "false")
	// starts_at/ends_at and issue_prep_start are already required for `live`;
	// set them so this test fails on the blocker it is about rather than on a
	// precondition it is not testing.
	if _, err := pool.Exec(ctx, `
UPDATE hackathons
SET announced_at = now() - interval '180 days',
    issue_prep_start = now() - interval '2 days',
    starts_at = now() - interval '1 day',
    ends_at = now() + interval '30 days'
WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set event dates: %v", err)
	}

	err := Transition(ctx, pool, hackathonID, "live", admin)
	if err == nil {
		t.Fatal("an event with zero published issues went live; it would advertise nothing and run no draw")
	}
	// Asserted on the message, because that is what Transition surfaces -
	// BlockingReason's key stays inside dynamicBlockers. Matching the key here
	// would have passed on any blocker at all.
	if !strings.Contains(err.Error(), "no published issues") {
		t.Fatalf("blocked for the wrong reason: %v", err)
	}

	// And it stops blocking once there is something to apply to - otherwise
	// this is a check nobody can satisfy.
	fxPublishedIssue(t, pool, hackathonID, projectID, 720, "standard")
	if err := Transition(ctx, pool, hackathonID, "live", admin); err != nil {
		t.Fatalf("still blocked after publishing an issue: %v", err)
	}
}
