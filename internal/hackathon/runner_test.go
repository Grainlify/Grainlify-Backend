package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// newTestRunner builds a runner with no GitHub client and no notifier, so
// the tick logic is exercised without any outbound calls. Both are optional
// by design - the Grainlify assignment is the source of truth (§2.3) and
// mirroring it out is best-effort.
func newTestRunner(t *testing.T) (*AssignmentRunner, context.Context) {
	t.Helper()
	d := dbtest.DB(t)
	return NewAssignmentRunner(d.Pool, nil, nil, nil), context.Background()
}

// TestRunner_RunDueDraws_AssignsWhenWindowClosed is the end-to-end
// deterministic path: published issue + applicants + closed window ->
// assignment, with zero model calls.
func TestRunner_RunDueDraws_AssignsWhenWindowClosed(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 400, "standard")
	fxApplicant(t, pool, hackathonID, issueID, "runner-a", "plausible")
	fxApplicant(t, pool, hackathonID, issueID, "runner-b", "plausible")
	fxCloseWindow(t, pool, issueID)

	if err := r.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}

	if n := countRows(t, pool, `
SELECT count(*) FROM hackathon_assignments WHERE hackathon_issue_id = $1 AND status = 'active'`, issueID); n != 1 {
		t.Errorf("active assignments = %d, want 1", n)
	}

	// A second tick must not double-assign - the query excludes issues that
	// already have an active assignment.
	if err := r.runDueDraws(ctx); err != nil {
		t.Fatalf("second runDueDraws: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_assignments WHERE hackathon_issue_id = $1`, issueID); n != 1 {
		t.Errorf("assignments after a second tick = %d, want 1", n)
	}
}

// TestRunner_RunDueDraws_EmptyWindowRetriesThenGivesUp covers §3.7's
// empty_window_retries: a quiet window reopens rather than burning the
// issue, but the retries terminate.
func TestRunner_RunDueDraws_EmptyWindowRetriesThenGivesUp(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "empty_window_retries", "2")
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 410, "standard")

	for i := 0; i < 2; i++ {
		fxCloseWindow(t, pool, issueID)
		if err := r.runDueDraws(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		var closesAt, now string
		if err := pool.QueryRow(ctx, `
SELECT application_window_closes_at::text, now()::text FROM hackathon_issues WHERE id = $1`, issueID).Scan(&closesAt, &now); err != nil {
			t.Fatalf("read window: %v", err)
		}
		if closesAt <= now {
			t.Errorf("tick %d: window was not reopened (closes_at %s <= now %s)", i, closesAt, now)
		}
	}

	// Retries are exhausted; the next closed window must not reopen again.
	fxCloseWindow(t, pool, issueID)
	if err := r.runDueDraws(ctx); err != nil {
		t.Fatalf("final tick: %v", err)
	}
	var reopened bool
	if err := pool.QueryRow(ctx, `
SELECT application_window_closes_at > now() FROM hackathon_issues WHERE id = $1`, issueID).Scan(&reopened); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if reopened {
		t.Error("window reopened past empty_window_retries - the retries never terminate")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_assignments WHERE hackathon_issue_id = $1`, issueID); n != 0 {
		t.Errorf("assignments for an issue nobody applied to = %d, want 0", n)
	}
}

// TestRunner_ReleaseStale_ReopensTheWindow: a released issue has to go back
// into the pool, or the abandon costs the issue as well as the contributor.
func TestRunner_ReleaseStale_ReopensTheWindow(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 420, "standard")
	fxCloseWindow(t, pool, issueID)

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "vanished")
	past := time.Now().Add(-time.Hour)
	fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 420, "vanished", &past)

	if err := r.releaseStale(ctx); err != nil {
		t.Fatalf("releaseStale: %v", err)
	}

	var reopened bool
	if err := pool.QueryRow(ctx, `
SELECT application_window_closes_at > now() FROM hackathon_issues WHERE id = $1`, issueID).Scan(&reopened); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !reopened {
		t.Error("window not reopened after a stale release, so the issue can never be re-drawn")
	}
}

// TestRunner_WarnEndOfEvent_OncePerAssignment: §13 #2 requires the warning
// go out before ends_at, and a runner ticking every minute must not resend
// it every minute.
func TestRunner_WarnEndOfEvent_OncePerAssignment(t *testing.T) {
	r, ctx := newTestRunner(t)
	pool := r.pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE hackathons SET ends_at = now() + interval '6 hours' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set ends_at: %v", err)
	}
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 430, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "about-to-lose-it")
	fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 430, "about-to-lose-it", nil)

	first, err := PendingEndOfEventWarning(ctx, pool, endOfEventWarningLead)
	if err != nil {
		t.Fatalf("first warning sweep: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first sweep returned %d assignments, want 1", len(first))
	}

	second, err := PendingEndOfEventWarning(ctx, pool, endOfEventWarningLead)
	if err != nil {
		t.Fatalf("second warning sweep: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep returned %d assignments, want 0 - the warning must not repeat every tick", len(second))
	}
}
