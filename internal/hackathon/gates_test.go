package hackathon

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// applicantFor builds the ApplicantContext CheckGates needs for a given
// issue and user.
func applicantFor(t *testing.T, pool db.DBPool, hackathonID, issueID, projectID, userID uuid.UUID, login string) ApplicantContext {
	t.Helper()
	var issueNumber int
	var orgLogin, fullName string
	if err := pool.QueryRow(context.Background(), `
SELECT hi.issue_number, hi.org_login, p.github_full_name
FROM hackathon_issues hi JOIN projects p ON p.id = hi.project_id
WHERE hi.id = $1
`, issueID).Scan(&issueNumber, &orgLogin, &fullName); err != nil {
		t.Fatalf("applicantFor: %v", err)
	}
	return ApplicantContext{
		HackathonID: hackathonID, IssueID: issueID, ProjectID: projectID,
		IssueNumber: issueNumber, OrgLogin: orgLogin, RepoFullName: fullName,
		UserID: userID, GitHubLogin: login,
	}
}

// gh is nil throughout: these tests cover the gates answerable from our own
// database, which are precisely the ones that must never be skipped.
func TestCheckGates_PassesForACleanApplicant(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 100, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "clean-applicant")

	res, err := CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, issueID, projectID, userID, "clean-applicant"))
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if !res.Passed {
		t.Errorf("clean applicant rejected by %q: %s", res.Gate, res.Reason)
	}
}

func TestCheckGates_BlocksIssueAuthor(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 101, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "the-author")

	ac := applicantFor(t, pool, hackathonID, issueID, projectID, userID, "the-author")
	ac.IssueAuthorLogin = "The-Author" // case-insensitive on purpose

	res, err := CheckGates(ctx, pool, nil, "", ac)
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed {
		t.Error("the issue author was allowed to apply to their own issue")
	}
	if res.Gate != "block_issue_author" {
		t.Errorf("gate = %q, want block_issue_author", res.Gate)
	}
}

func TestCheckGates_SlotAvailability(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "slots_per_contributor", "1")

	held := fxPublishedIssue(t, pool, hackathonID, projectID, 110, "standard")
	target := fxPublishedIssue(t, pool, hackathonID, projectID, 111, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "slot-holder")
	assignmentID := fxAssignment(t, pool, hackathonID, held, projectID, userID, 110, "slot-holder", nil)

	ac := applicantFor(t, pool, hackathonID, target, projectID, userID, "slot-holder")
	res, err := CheckGates(ctx, pool, nil, "", ac)
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed {
		t.Error("applicant with no free slot was allowed to apply")
	}
	if res.Gate != "slot_availability" {
		t.Errorf("gate = %q, want slot_availability", res.Gate)
	}

	// Freeing the slot (as a qualifying PR would) must re-open applications
	// even though the assignment itself is still open - that's the whole
	// point of slot_freed_on = pr_submission.
	if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET holds_slot = false WHERE id = $1`, assignmentID); err != nil {
		t.Fatalf("free slot: %v", err)
	}
	res, err = CheckGates(ctx, pool, nil, "", ac)
	if err != nil {
		t.Fatalf("CheckGates after freeing: %v", err)
	}
	if !res.Passed {
		t.Errorf("after the slot was freed, application still rejected by %q: %s", res.Gate, res.Reason)
	}
}

func TestCheckGates_AbandonLockout(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "abandons_before_lockout", "2")

	oldIssue := fxPublishedIssue(t, pool, hackathonID, projectID, 120, "standard")
	target := fxPublishedIssue(t, pool, hackathonID, projectID, 121, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "serial-abandoner")

	for i := 0; i < 2; i++ {
		id := fxAssignment(t, pool, hackathonID, oldIssue, projectID, userID, 120, "serial-abandoner", nil)
		if _, err := pool.Exec(ctx, `
UPDATE hackathon_assignments SET status = 'released_stale', holds_slot = false, abandon_recorded = true WHERE id = $1
`, id); err != nil {
			t.Fatalf("record abandon: %v", err)
		}
	}

	res, err := CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, target, projectID, userID, "serial-abandoner"))
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed {
		t.Error("applicant at the abandon limit was allowed to apply")
	}
	if res.Gate != "abandon_lockout" {
		t.Errorf("gate = %q, want abandon_lockout", res.Gate)
	}
}

// TestCheckGates_MaxConcurrentApplications covers AI-specs.md §13's first
// open question as answered: applications are free, slots consumed only on
// winning, so the cap is what stops one account entering every pool.
func TestCheckGates_MaxConcurrentApplications(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "max_concurrent_applications", "2")

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "farmer")
	for i := 0; i < 2; i++ {
		iss := fxPublishedIssue(t, pool, hackathonID, projectID, 130+i, "standard")
		fxApplication(t, pool, hackathonID, iss, userID, "farmer", "plausible")
	}

	target := fxPublishedIssue(t, pool, hackathonID, projectID, 140, "standard")
	res, err := CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, target, projectID, userID, "farmer"))
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed {
		t.Error("third concurrent application allowed despite a cap of 2")
	}
	if res.Gate != "max_concurrent_applications" {
		t.Errorf("gate = %q, want max_concurrent_applications", res.Gate)
	}

	// A resolved application frees capacity: the cap is on *open*
	// applications, not lifetime ones.
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issue_applications SET status = 'lost'
WHERE hackathon_id = $1 AND user_id = $2 AND status = 'applied'
`, hackathonID, userID); err != nil {
		t.Fatalf("resolve applications: %v", err)
	}
	res, err = CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, target, projectID, userID, "farmer"))
	if err != nil {
		t.Fatalf("CheckGates after resolving: %v", err)
	}
	if !res.Passed {
		t.Errorf("after earlier applications resolved, still rejected by %q: %s", res.Gate, res.Reason)
	}
}

func TestCheckGates_OrgCap(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "max_issues_per_contributor_per_org", "1")
	fxSetConfig(t, pool, hackathonID, "slots_per_contributor", "5") // isolate the org cap

	won := fxPublishedIssue(t, pool, hackathonID, projectID, 150, "standard")
	target := fxPublishedIssue(t, pool, hackathonID, projectID, 151, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "org-capper")
	id := fxAssignment(t, pool, hackathonID, won, projectID, userID, 150, "org-capper", nil)
	if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET status = 'completed', holds_slot = false WHERE id = $1`, id); err != nil {
		t.Fatalf("complete assignment: %v", err)
	}

	res, err := CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, target, projectID, userID, "org-capper"))
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed {
		t.Error("applicant over the per-org cap was allowed to apply")
	}
	if res.Gate != "org_cap" {
		t.Errorf("gate = %q, want org_cap", res.Gate)
	}
}

func TestCheckGates_RejectsWhenHackathonNotLiveOrIssueUnpublished(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 160, "standard")
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "early-bird")
	ac := applicantFor(t, pool, hackathonID, issueID, projectID, userID, "early-bird")

	if _, err := pool.Exec(ctx, `UPDATE hackathon_issues SET status = 'pending' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	res, err := CheckGates(ctx, pool, nil, "", ac)
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed || res.Gate != "hackathon_context" {
		t.Errorf("unpublished issue: passed=%v gate=%q, want a hackathon_context rejection", res.Passed, res.Gate)
	}

	if _, err := pool.Exec(ctx, `UPDATE hackathon_issues SET status = 'published' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hackathons SET phase = 'issue_prep' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("set phase: %v", err)
	}
	res, err = CheckGates(ctx, pool, nil, "", ac)
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed || res.Gate != "hackathon_context" {
		t.Errorf("non-live hackathon: passed=%v gate=%q, want a hackathon_context rejection", res.Passed, res.Gate)
	}
}

func TestCheckGates_ApplicationWindowClosed(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 170, "standard")
	fxCloseWindow(t, pool, issueID)
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "latecomer")

	res, err := CheckGates(ctx, pool, nil, "", applicantFor(t, pool, hackathonID, issueID, projectID, userID, "latecomer"))
	if err != nil {
		t.Fatalf("CheckGates: %v", err)
	}
	if res.Passed || res.Gate != "application_window" {
		t.Errorf("passed=%v gate=%q, want an application_window rejection", res.Passed, res.Gate)
	}
}

func TestEffectiveSlots_EarnedSlots(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	fxSetConfig(t, pool, hackathonID, "slots_per_contributor", "2")
	fxSetConfig(t, pool, hackathonID, "earned_slots_enabled", "true")
	fxSetConfig(t, pool, hackathonID, "earned_slots_threshold", "2")
	fxSetConfig(t, pool, hackathonID, "earned_slots_max", "3")

	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, "earner")
	env, err := loadGateEnv(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("loadGateEnv: %v", err)
	}

	got, err := EffectiveSlots(ctx, pool, hackathonID, userID, env)
	if err != nil {
		t.Fatalf("EffectiveSlots: %v", err)
	}
	if got != 2 {
		t.Errorf("slots with no completions = %d, want 2", got)
	}

	for i := 0; i < 3; i++ {
		iss := fxPublishedIssue(t, pool, hackathonID, projectID, 180+i, "standard")
		id := fxAssignment(t, pool, hackathonID, iss, projectID, userID, 180+i, "earner", nil)
		if _, err := pool.Exec(ctx, `UPDATE hackathon_assignments SET status = 'completed', holds_slot = false WHERE id = $1`, id); err != nil {
			t.Fatalf("complete: %v", err)
		}
	}

	got, err = EffectiveSlots(ctx, pool, hackathonID, userID, env)
	if err != nil {
		t.Fatalf("EffectiveSlots: %v", err)
	}
	if got != 3 {
		t.Errorf("slots after 3 completions = %d, want 3 (capped by earned_slots_max)", got)
	}
}
