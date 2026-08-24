package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// Walks one hackathon through every phase with the AI flags off, asserting
// the transitions actually chain and nothing is stranded between them.
//
// Every individual stage has its own tests. This one exists because the
// lifecycle had never been run end to end - the failure it is looking for is
// a phase that cannot be reached, or state that a transition leaves behind.
func TestLifecycle_EndToEndWithAIOff(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool

	admin := fxUser(t, pool)
	owner := fxUser(t, pool)
	projectID := fxProject(t, pool, owner, "")
	hackathonID := fxHackathon(t, pool, fxHackathonSpec{Phase: "draft"})

	if _, err := pool.Exec(ctx, `
UPDATE hackathons
SET announced_at = now() - interval '90 days',
    application_period_start = now() - interval '60 days',
    application_period_end   = now() - interval '40 days',
    issue_prep_start         = now() - interval '30 days',
    starts_at                = now() - interval '10 days',
    ends_at                  = now() + interval '10 days',
    contributor_prize_pool   = 1000,
    maintainer_prize_pool    = 500
WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("seed dates and pools: %v", err)
	}

	advance := func(to string) {
		t.Helper()
		if err := Transition(ctx, pool, hackathonID, to, admin); err != nil {
			blocking, next, rerr := Readiness(ctx, pool, hackathonID)
			t.Fatalf("could not advance to %q: %v (next=%q blocking=%+v readinessErr=%v)", to, err, next, blocking, rerr)
		}
	}
	phaseNow := func() string {
		t.Helper()
		var p string
		if err := pool.QueryRow(ctx, `SELECT phase FROM hackathons WHERE id = $1`, hackathonID).Scan(&p); err != nil {
			t.Fatalf("read phase: %v", err)
		}
		return p
	}

	// Phase 0 -> 1 -> 2.
	advance("application_period")
	fxAcceptedApplication(t, pool, hackathonID, projectID, owner)
	advance("issue_prep")

	// Phase 2 -> 3. An event that publishes results to contributors has to
	// leave shadow mode first, and going live is where that is enforced -
	// this walkthrough is the real path, so it does what a real admin does.
	if err := SetValue(ctx, pool, &hackathonID, "judging_shadow_mode", "false", admin); err != nil {
		t.Fatalf("leave shadow mode: %v", err)
	}

	// An event needs something to apply to before it can go live (#490).
	// Publishing an issue is what a real admin does in issue_prep, and this
	// walkthrough is the real path.
	fxPublishedIssue(t, pool, hackathonID, projectID, 900, "standard")

	// The config snapshot is taken here and is what the event reads from now
	// on.
	advance("live")
	var snapshotTaken *string
	if err := pool.QueryRow(ctx,
		`SELECT config_snapshot_taken_at::text FROM hackathons WHERE id = $1`, hackathonID).Scan(&snapshotTaken); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapshotTaken == nil {
		t.Fatal("going live did not freeze the config snapshot; a live event would read mutable global config")
	}

	// Contribution history, so the maintainer score has something to measure.
	// Without it the repo legitimately scores zero on every criterion and the
	// pool allocates nothing - correct, but not a realistic dry run.
	if _, err := pool.Exec(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login, created_at_github, merged)
SELECT $1, 900000 + g, 900000 + g, 'closed', 'oldtimer-' || g, now() - interval '200 days', true
FROM generate_series(1, 3) g`, projectID); err != nil {
		t.Fatalf("seed pre-event history: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login, created_at_github, merged)
SELECT $1, 910000 + g, 910000 + g, 'closed', 'newcomer-' || g, now() - interval '5 days', true
FROM generate_series(1, 12) g`, projectID); err != nil {
		t.Fatalf("seed event-window contributors: %v", err)
	}

	// An issue, an assignment, and a merged PR that becomes a verdict.
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 5001, "standard")
	contributor := fxUser(t, pool)
	asgID := fxAssignment(t, pool, hackathonID, issueID, projectID, contributor, 5001, "contrib", nil)

	// The contributor rates issue clarity - allowed while results are unpublished.
	if err := SubmitClarityRating(ctx, pool, asgID, contributor, 4, "clear enough"); err != nil {
		t.Fatalf("clarity rating during the event should be accepted: %v", err)
	}

	var verdictID uuid.UUID
	if err := pool.QueryRow(ctx, `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, hackathon_issue_id, assignment_id, pr_number, user_id,
   github_login, prefilter_status)
VALUES ($1,$2,$3,$4,5001,$5,'contrib','passed')
RETURNING id`, hackathonID, projectID, issueID, asgID, contributor).Scan(&verdictID); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	// Phase 3 -> 4. Closing releases in-flight assignments.
	advance("closed")
	if phaseNow() != "closed" {
		t.Fatalf("phase = %q after closing", phaseNow())
	}

	// Phase 4 -> 5 is blocked while any qualifying PR has no bucket.
	if err := Transition(ctx, pool, hackathonID, "results_published", admin); err == nil {
		t.Fatal("published results with an unjudged PR; contributors would be invited to appeal a result that does not exist")
	}

	if _, err := pool.Exec(ctx, `
UPDATE hackathon_verdicts SET final_bucket = 'substantial', final_source = 'auto_confirmed'
WHERE id = $1`, verdictID); err != nil {
		t.Fatalf("judge verdict: %v", err)
	}
	advance("results_published")

	// The appeal window opens on publication.
	window, err := GetAppealWindowByID(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("appeal window: %v", err)
	}
	if !window.Open || window.OpensAt == nil {
		t.Fatalf("appeal window did not open at publication: %+v", window)
	}

	// Ratings close when results publish.
	issue2 := fxPublishedIssue(t, pool, hackathonID, projectID, 5002, "standard")
	c2 := fxUser(t, pool)
	asg2 := fxAssignment(t, pool, hackathonID, issue2, projectID, c2, 5002, "late", nil)
	if err := SubmitClarityRating(ctx, pool, asg2, c2, 5, ""); err == nil {
		t.Error("a clarity rating was accepted after results were published")
	}

	// An appeal blocks settling until answered.
	appealID, err := SubmitAppeal(ctx, pool, SubmitAppealInput{
		VerdictID: verdictID, UserID: contributor, Reason: "should have been exceptional",
	})
	if err != nil {
		t.Fatalf("SubmitAppeal: %v", err)
	}
	if err := Transition(ctx, pool, hackathonID, "settled", admin); err == nil {
		t.Fatal("settled with an appeal still pending; someone would be paid before their appeal was answered")
	}
	if err := DecideAppeal(ctx, pool, AppealDecision{
		AppealID: appealID, ReviewerID: admin, Upheld: true, NewBucket: "exceptional",
		Reason: "reworked the retry path, not a config change",
	}); err != nil {
		t.Fatalf("DecideAppeal: %v", err)
	}

	// Still blocked until the window elapses.
	if err := Transition(ctx, pool, hackathonID, "settled", admin); err == nil {
		t.Fatal("settled while the appeal window was still open")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE hackathons SET results_published_at = now() - interval '30 days' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("age the window: %v", err)
	}

	// Phase 5 -> 6.
	advance("settled")
	if phaseNow() != "settled" {
		t.Fatalf("phase = %q after settling", phaseNow())
	}

	// The post-appeal recompute ran, and the payout reflects the upheld appeal.
	var appealsClosed *string
	var payout *float64
	if err := pool.QueryRow(ctx, `
SELECT h.appeals_closed_at::text, v.payout_amount::float8
FROM hackathons h JOIN hackathon_verdicts v ON v.hackathon_id = h.id
WHERE h.id = $1 AND v.id = $2`, hackathonID, verdictID).Scan(&appealsClosed, &payout); err != nil {
		t.Fatalf("read settle results: %v", err)
	}
	if appealsClosed == nil {
		t.Fatal("settling did not close out the appeal window, so the recompute never ran")
	}
	if payout == nil || *payout <= 0 {
		t.Fatalf("verdict payout = %v after settle, want the whole pool on the only funded PR", payout)
	}

	// The maintainer pool settled onto its own row, from its own budget.
	var mScore, mGross, mHold float64
	var due *string
	if err := pool.QueryRow(ctx, `
SELECT score::float8, gross_amount::float8, holdback_amount::float8, holdback_due_at::text
FROM hackathon_maintainer_payouts WHERE hackathon_id = $1 AND project_id = $2`,
		hackathonID, projectID).Scan(&mScore, &mGross, &mHold, &due); err != nil {
		t.Fatalf("maintainer payout row missing after settle: %v", err)
	}
	if mGross <= 0 || mGross > 500 {
		t.Errorf("maintainer gross = %v, want >0 and within the 500 maintainer pool", mGross)
	}
	if mHold <= 0 {
		t.Errorf("holdback = %v, want the configured share withheld", mHold)
	}
	if due == nil {
		t.Error("no holdback due date set, so the release job would never select it")
	}

	// Money cannot move while in shadow mode. This event left shadow mode to
	// go live, so put it back deliberately for the assertion: an admin can
	// still edit config on a live event (§1.1), and the guard reads the live
	// value rather than the frozen snapshot, so re-entering it mid-event is
	// a state the system can genuinely be in.
	if err := SetValue(ctx, pool, &hackathonID, "judging_shadow_mode", "true", admin); err != nil {
		t.Fatalf("re-enter shadow mode: %v", err)
	}
	if err := GuardPayoutRelease(ctx, pool, PayoutReleaseRequest{
		HackathonID: hackathonID, PayoutRunID: uuid.New(), ActorID: admin, Confirm: true,
	}); err == nil {
		t.Error("payout released while in shadow mode")
	}
	if err := SetValue(ctx, pool, &hackathonID, "judging_shadow_mode", "false", admin); err != nil {
		t.Fatalf("leave shadow mode again: %v", err)
	}

	// The holdback resolves once due, conditional on activity.
	if _, err := pool.Exec(ctx,
		`UPDATE hackathon_maintainer_payouts SET holdback_due_at = now() - interval '1 day' WHERE hackathon_id = $1`,
		hackathonID); err != nil {
		t.Fatalf("age the holdback: %v", err)
	}
	// No activity, but measured - so it resolves as withheld rather than
	// hanging pending forever.
	resolved, err := ReleaseDueHoldbacks(ctx, pool, func(_ context.Context, _ uuid.UUID, _ string, since time.Time, windowDays int) RepoActivity {
		return RepoActivity{WindowDays: windowDays, Since: since, CommitsMeasured: true}
	})
	if err != nil {
		t.Fatalf("ReleaseDueHoldbacks: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("resolved %d holdbacks, want 1 - a due holdback was stranded", resolved)
	}
	var hbStatus, destination string
	if err := pool.QueryRow(ctx, `
SELECT holdback_status, COALESCE(withheld_destination, '')
FROM hackathon_maintainer_payouts WHERE hackathon_id = $1`, hackathonID).Scan(&hbStatus, &destination); err != nil {
		t.Fatalf("read holdback: %v", err)
	}
	if hbStatus != "withheld" {
		t.Errorf("holdback status = %q, want withheld for a repo with no activity", hbStatus)
	}
	if destination == "" {
		t.Error("withheld money has no recorded destination")
	}
}
