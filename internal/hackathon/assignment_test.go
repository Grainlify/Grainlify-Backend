package hackathon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func assignmentState(t *testing.T, pool db.DBPool, id uuid.UUID) (status string, holdsSlot, abandon bool, staleAt *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
SELECT status, holds_slot, abandon_recorded, stale_at FROM hackathon_assignments WHERE id = $1
`, id).Scan(&status, &holdsSlot, &abandon, &staleAt); err != nil {
		t.Fatalf("assignmentState: %v", err)
	}
	return
}

// TestRecordQualifyingPR_FreesSlotAndStopsStaleTimer covers §4.6's
// "Qualifying PR submitted → slot freed immediately, timer stops".
func TestRecordQualifyingPR_FreesSlotAndStopsStaleTimer(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 200, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "pr-submitter")
	stale := time.Now().Add(48 * time.Hour)
	aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 200, "pr-submitter", &stale)

	if err := RecordQualifyingPR(ctx, pool, hackathonID, projectID, 200, 4242, "PR-Submitter"); err != nil {
		t.Fatalf("RecordQualifyingPR: %v", err)
	}

	status, holdsSlot, abandon, staleAt := assignmentState(t, pool, aID)
	if status != "pr_submitted" {
		t.Errorf("status = %q, want pr_submitted", status)
	}
	if holdsSlot {
		t.Error("slot still held after a qualifying PR (slot_freed_on defaults to pr_submission)")
	}
	if abandon {
		t.Error("submitting a PR must never record an abandon")
	}
	if staleAt != nil {
		t.Error("stale timer should be cleared once a qualifying PR exists, so review latency can't cost the contributor their assignment")
	}
}

// With slot_freed_on = pr_merge the slot must stay held at submission.
func TestRecordQualifyingPR_HoldsSlotWhenFreedOnMerge(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "slot_freed_on", "pr_merge")
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 201, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "merge-waiter")
	aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 201, "merge-waiter", nil)

	if err := RecordQualifyingPR(ctx, pool, hackathonID, projectID, 201, 1, "merge-waiter"); err != nil {
		t.Fatalf("RecordQualifyingPR: %v", err)
	}
	if _, holdsSlot, _, _ := assignmentState(t, pool, aID); !holdsSlot {
		t.Error("slot freed at submission despite slot_freed_on = pr_merge")
	}

	if err := RecordMerge(ctx, pool, hackathonID, projectID, 201, "merge-waiter"); err != nil {
		t.Fatalf("RecordMerge: %v", err)
	}
	status, holdsSlot, _, _ := assignmentState(t, pool, aID)
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
	if holdsSlot {
		t.Error("slot still held after merge")
	}
}

// TestReleaseVoluntary_GraceWindow covers §4.6's two voluntary-release rows.
func TestReleaseVoluntary_GraceWindow(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "voluntary_release_grace_hours", "48")

	t.Run("inside the grace window records no abandon", func(t *testing.T) {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 210, "standard")
		userID := fxUser(t, pool)
		fxGitHubAccount(t, pool, userID, "quick-returner")
		aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 210, "quick-returner", nil)

		abandoned, err := ReleaseVoluntary(ctx, pool, hackathonID, userID, aID)
		if err != nil {
			t.Fatalf("ReleaseVoluntary: %v", err)
		}
		if abandoned {
			t.Error("release inside the grace window recorded an abandon")
		}
		status, holdsSlot, abandon, _ := assignmentState(t, pool, aID)
		if status != "released_voluntary" || holdsSlot || abandon {
			t.Errorf("state = (%s, holds=%v, abandon=%v), want (released_voluntary, false, false)", status, holdsSlot, abandon)
		}
	})

	t.Run("after the grace window records an abandon", func(t *testing.T) {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 211, "standard")
		userID := fxUser(t, pool)
		fxGitHubAccount(t, pool, userID, "slow-returner")
		aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 211, "slow-returner", nil)
		if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET assigned_at = now() - interval '72 hours' WHERE id = $1`, aID); err != nil {
			t.Fatalf("backdate: %v", err)
		}

		abandoned, err := ReleaseVoluntary(ctx, pool, hackathonID, userID, aID)
		if err != nil {
			t.Fatalf("ReleaseVoluntary: %v", err)
		}
		if !abandoned {
			t.Error("release after the grace window did not record an abandon")
		}
		if _, _, abandon, _ := assignmentState(t, pool, aID); !abandon {
			t.Error("abandon_recorded not persisted")
		}
	})

	t.Run("releasing someone else's assignment fails", func(t *testing.T) {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 212, "standard")
		owner := fxUser(t, pool)
		fxGitHubAccount(t, pool, owner, "assignment-owner")
		aID := fxAssignment(t, pool, hackathonID, issueID, projectID, owner, 212, "assignment-owner", nil)
		stranger := fxUser(t, pool)

		if _, err := ReleaseVoluntary(ctx, pool, hackathonID, stranger, aID); !errors.Is(err, ErrNoActiveAssignment) {
			t.Errorf("err = %v, want ErrNoActiveAssignment", err)
		}
	})
}

// TestReleaseStale covers §4.6's "Stale timeout → auto-released, slot freed,
// abandon recorded", and that a submitted PR protects against it.
func TestReleaseStale(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	expired := fxPublishedIssue(t, pool, hackathonID, projectID, 220, "standard")
	expiredUser := fxUser(t, pool)
	fxGitHubAccount(t, pool, expiredUser, "gone-quiet")
	past := time.Now().Add(-time.Hour)
	expiredID := fxAssignment(t, pool, hackathonID, expired, projectID, expiredUser, 220, "gone-quiet", &past)

	fresh := fxPublishedIssue(t, pool, hackathonID, projectID, 221, "standard")
	freshUser := fxUser(t, pool)
	fxGitHubAccount(t, pool, freshUser, "still-working")
	future := time.Now().Add(72 * time.Hour)
	freshID := fxAssignment(t, pool, hackathonID, fresh, projectID, freshUser, 221, "still-working", &future)

	released, err := ReleaseStale(ctx, pool)
	if err != nil {
		t.Fatalf("ReleaseStale: %v", err)
	}

	var sawExpired bool
	for _, r := range released {
		if r.AssignmentID == expiredID {
			sawExpired = true
		}
		if r.AssignmentID == freshID {
			t.Error("released an assignment whose stale deadline hasn't passed")
		}
	}
	if !sawExpired {
		t.Fatal("the expired assignment was not released")
	}

	status, holdsSlot, abandon, _ := assignmentState(t, pool, expiredID)
	if status != "released_stale" || holdsSlot || !abandon {
		t.Errorf("expired state = (%s, holds=%v, abandon=%v), want (released_stale, false, true)", status, holdsSlot, abandon)
	}
	if status, holdsSlot, _, _ := assignmentState(t, pool, freshID); status != "active" || !holdsSlot {
		t.Errorf("fresh assignment disturbed: (%s, holds=%v)", status, holdsSlot)
	}
}

// TestCloseEventAssignments covers AI-specs.md §13 #2's second half: at
// event close, open assignments are released and the issue reverts.
func TestCloseEventAssignments(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	openIssue := fxPublishedIssue(t, pool, hackathonID, projectID, 230, "standard")
	openUser := fxUser(t, pool)
	fxGitHubAccount(t, pool, openUser, "in-flight")
	openID := fxAssignment(t, pool, hackathonID, openIssue, projectID, openUser, 230, "in-flight", nil)

	doneIssue := fxPublishedIssue(t, pool, hackathonID, projectID, 231, "standard")
	doneUser := fxUser(t, pool)
	fxGitHubAccount(t, pool, doneUser, "finished")
	doneID := fxAssignment(t, pool, hackathonID, doneIssue, projectID, doneUser, 231, "finished", nil)
	if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET status = 'completed', holds_slot = false WHERE id = $1`, doneID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// An application still open when the event closes is moot.
	pendingIssue := fxPublishedIssue(t, pool, hackathonID, projectID, 232, "standard")
	fxApplicant(t, pool, hackathonID, pendingIssue, "never-drawn", "plausible")

	released, err := CloseEventAssignments(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("CloseEventAssignments: %v", err)
	}
	if len(released) != 1 || released[0].AssignmentID != openID {
		t.Errorf("released = %+v, want just the in-flight assignment so the caller can unassign it on GitHub", released)
	}

	status, holdsSlot, abandon, _ := assignmentState(t, pool, openID)
	if status != "released_event_end" || holdsSlot {
		t.Errorf("in-flight assignment = (%s, holds=%v), want (released_event_end, false)", status, holdsSlot)
	}
	if abandon {
		t.Error("the event ending is not the contributor's fault and must not record an abandon")
	}
	if status, _, _, _ := assignmentState(t, pool, doneID); status != "completed" {
		t.Errorf("completed assignment changed to %q", status)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM hackathon_issue_applications WHERE hackathon_id = $1 AND status = 'applied'`, hackathonID); n != 0 {
		t.Errorf("open applications after close = %d, want 0", n)
	}

	// §13 #2 is "release AND revert": an unfinished issue must leave the
	// hackathon, not sit orphaned in 'published' with a dead window.
	var openIssueStatus string
	var opensAt, closesAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT status, application_window_opens_at, application_window_closes_at FROM hackathon_issues WHERE id = $1
`, openIssue).Scan(&openIssueStatus, &opensAt, &closesAt); err != nil {
		t.Fatalf("read reverted issue: %v", err)
	}
	if openIssueStatus != "removed" {
		t.Errorf("unfinished issue status = %q, want removed - it should revert to a normal Grainlify bounty", openIssueStatus)
	}
	if opensAt != nil || closesAt != nil {
		t.Error("reverted issue still has an application window, which nothing will ever draw")
	}

	// The issue that was actually completed stays published for judging.
	var doneIssueStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM hackathon_issues WHERE id = $1`, doneIssue).Scan(&doneIssueStatus); err != nil {
		t.Fatalf("read completed issue: %v", err)
	}
	if doneIssueStatus != "published" {
		t.Errorf("completed issue status = %q, want published - judging still needs it", doneIssueStatus)
	}

	// An issue nobody ever won reverts too.
	var pendingStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM hackathon_issues WHERE id = $1`, pendingIssue).Scan(&pendingStatus); err != nil {
		t.Fatalf("read never-drawn issue: %v", err)
	}
	if pendingStatus != "removed" {
		t.Errorf("never-drawn issue status = %q, want removed", pendingStatus)
	}
}

// TestTransitionToClosed_ReleasesInFlight checks the phase hook end to end,
// which is what makes 'closed' more than a label.
func TestTransitionToClosed_ReleasesInFlight(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	admin := fxAdmin(t, pool)

	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 240, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "closing-time")
	aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 240, "closing-time", nil)

	if err := Transition(ctx, pool, hackathonID, "closed", admin); err != nil {
		t.Fatalf("Transition to closed: %v", err)
	}

	var phase string
	if err := pool.QueryRow(ctx, `SELECT phase FROM hackathons WHERE id = $1`, hackathonID).Scan(&phase); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if phase != "closed" {
		t.Errorf("phase = %q, want closed", phase)
	}
	if status, holdsSlot, _, _ := assignmentState(t, pool, aID); status != "released_event_end" || holdsSlot {
		t.Errorf("assignment after close = (%s, holds=%v), want (released_event_end, false)", status, holdsSlot)
	}
}

// TestActiveAssignee_DrivesIsAssigned is the behaviour that unblocked
// intake.go's stub: pulling the label off an assigned issue must flag for
// admin rather than silently discard in-flight work.
func TestActiveAssignee_DrivesIsAssigned(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 250, "standard")

	if login, err := ActiveAssignee(ctx, pool, projectID, 250); err != nil || login != "" {
		t.Errorf("unassigned issue: login=%q err=%v, want empty", login, err)
	}
	if isAssigned(ctx, pool, hackathonID, projectID, 250) {
		t.Error("isAssigned true for an unassigned issue")
	}

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "holder")
	aID := fxAssignment(t, pool, hackathonID, issueID, projectID, userID, 250, "holder", nil)

	if login, err := ActiveAssignee(ctx, pool, projectID, 250); err != nil || login != "holder" {
		t.Errorf("assigned issue: login=%q err=%v, want \"holder\"", login, err)
	}
	if !isAssigned(ctx, pool, hackathonID, projectID, 250) {
		t.Error("isAssigned false for an actively assigned issue")
	}

	// Once released, the issue is free again.
	if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET status = 'released_stale', holds_slot = false WHERE id = $1`, aID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if isAssigned(ctx, pool, hackathonID, projectID, 250) {
		t.Error("isAssigned still true after the assignment was released")
	}
}
