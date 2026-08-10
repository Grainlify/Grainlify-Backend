package hackathon

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// apFxVerdict inserts a judged verdict owned by userID.
func apFxVerdict(t *testing.T, pool db.DBPool, hackathonID, projectID uuid.UUID, userID *uuid.UUID, pr int, bucket string) uuid.UUID {
	t.Helper()
	return apFxVerdictAs(t, pool, hackathonID, projectID, userID, pr, bucket, "octocat")
}

// apFxVerdictAs is the same with an explicit login. The diminishing-returns
// curve groups by login, so tests that mean "two different people" have to
// say so - a shared login makes their PRs one contributor's sequence.
func apFxVerdictAs(t *testing.T, pool db.DBPool, hackathonID, projectID uuid.UUID, userID *uuid.UUID, pr int, bucket, login string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, pr_number, user_id, github_login,
   prefilter_status, final_bucket, final_source)
VALUES ($1,$2,$3,$4,$6,'passed',$5,'auto_confirmed')
RETURNING id
`, hackathonID, projectID, pr, userID, bucket, login).Scan(&id)
	if err != nil {
		t.Fatalf("apFxVerdict: %v", err)
	}
	return id
}

// apFxPublishResults moves a hackathon to results_published with the window
// anchored daysAgo days back, so tests can put themselves inside or outside it.
func apFxPublishResults(t *testing.T, pool db.DBPool, hackathonID uuid.UUID, daysAgo int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE hackathons
SET phase = 'results_published',
    results_published_at = now() - make_interval(days => $2)
WHERE id = $1
`, hackathonID, daysAgo); err != nil {
		t.Fatalf("apFxPublishResults: %v", err)
	}
}

func apFxSetPool(t *testing.T, pool db.DBPool, hackathonID uuid.UUID, amount float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hackathons SET contributor_prize_pool = $2 WHERE id = $1`, hackathonID, amount); err != nil {
		t.Fatalf("apFxSetPool: %v", err)
	}
}

func TestSubmitAppeal_RequiresOwnVerdictAnOpenWindowAndAReason(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	owner := fxUser(t, pool)
	stranger := fxUser(t, pool)
	verdictID := apFxVerdict(t, pool, hackathonID, projectID, &owner, 900, "accepted")

	// Window not open yet: the hackathon is still live.
	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "please look again"}); !errors.Is(err, ErrAppealWindowClosed) {
		t.Fatalf("appeal before results were published: err = %v, want ErrAppealWindowClosed", err)
	}

	apFxPublishResults(t, pool, hackathonID, 1)

	// Someone else's verdict is not appealable by this user, and the error
	// must not depend on the window being open.
	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: stranger, Reason: "mine actually"}); !errors.Is(err, ErrAppealNotYours) {
		t.Fatalf("appeal against another user's verdict: err = %v, want ErrAppealNotYours", err)
	}

	// A blank reason gives the reviewer nothing to answer.
	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "   "}); err == nil {
		t.Fatal("expected a blank appeal reason to be rejected")
	}

	id, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "the tests I added were not counted"})
	if err != nil {
		t.Fatalf("SubmitAppeal: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("SubmitAppeal returned a nil id")
	}

	// One appeal per verdict.
	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "again"}); !errors.Is(err, ErrAppealExists) {
		t.Fatalf("second appeal: err = %v, want ErrAppealExists", err)
	}
}

func TestSubmitAppeal_RejectedOnceTheWindowHasElapsed(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	owner := fxUser(t, pool)
	verdictID := apFxVerdict(t, pool, hackathonID, projectID, &owner, 901, "accepted")

	// Default appeal_window_days is 7; publish 8 days ago.
	apFxPublishResults(t, pool, hackathonID, 8)

	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "too late"}); !errors.Is(err, ErrAppealWindowClosed) {
		t.Fatalf("appeal after the window: err = %v, want ErrAppealWindowClosed", err)
	}
}

func TestDecideAppeal_RequiresAReasonAndIsFinal(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	owner := fxUser(t, pool)
	reviewer := fxUser(t, pool)
	verdictID := apFxVerdict(t, pool, hackathonID, projectID, &owner, 902, "accepted")
	apFxPublishResults(t, pool, hackathonID, 1)

	appealID, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "bucket is too low"})
	if err != nil {
		t.Fatalf("SubmitAppeal: %v", err)
	}

	if err := DecideAppeal(ctx, pool, AppealDecision{AppealID: appealID, ReviewerID: reviewer, Upheld: true, NewBucket: "substantial", Reason: ""}); err == nil {
		t.Fatal("expected a decision with no written reason to be rejected")
	}
	if err := DecideAppeal(ctx, pool, AppealDecision{AppealID: appealID, ReviewerID: reviewer, Upheld: false, NewBucket: "substantial", Reason: "no"}); err == nil {
		t.Fatal("expected a rejected appeal that also changes the bucket to be refused")
	}

	if err := DecideAppeal(ctx, pool, AppealDecision{
		AppealID: appealID, ReviewerID: reviewer, Upheld: true, NewBucket: "substantial",
		Reason: "the migration counts as schema work, which the rubric puts above accepted",
	}); err != nil {
		t.Fatalf("DecideAppeal: %v", err)
	}

	// The upheld appeal must move the verdict through the same override path
	// an admin override uses, so there is one place a final bucket changes.
	var bucket, source, overrideReason string
	if err := pool.QueryRow(ctx, `
SELECT final_bucket, final_source, override_reason FROM hackathon_verdicts WHERE id = $1
`, verdictID).Scan(&bucket, &source, &overrideReason); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	if bucket != "substantial" {
		t.Errorf("final_bucket = %q, want substantial", bucket)
	}
	if source != "human_override" {
		t.Errorf("final_source = %q, want human_override", source)
	}
	if overrideReason == "" {
		t.Error("override_reason is empty; the appeal reason should have been carried onto the verdict")
	}

	// A decision is final.
	if err := DecideAppeal(ctx, pool, AppealDecision{
		AppealID: appealID, ReviewerID: reviewer, Upheld: false, Reason: "changed my mind",
	}); !errors.Is(err, ErrAppealDecided) {
		t.Fatalf("second decision: err = %v, want ErrAppealDecided", err)
	}
}

// The §13-#4 rule: an upheld appeal changes total_units, so unit_value must be
// recomputed for everyone - not just for the appellant - or the payouts stop
// summing to the advertised pool.
func TestCloseAppealsAndRecompute_RedividesThePoolForEveryone(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	apFxSetPool(t, pool, hackathonID, 1000)

	appellant := fxUser(t, pool)
	// Four accepted PRs at 1 unit each = 4 units, $250 apiece.
	appealVerdict := apFxVerdictAs(t, pool, hackathonID, projectID, &appellant, 910, "accepted", "appellant")
	other := fxUser(t, pool)
	apFxVerdictAs(t, pool, hackathonID, projectID, &other, 911, "accepted", "bystander")
	apFxVerdictAs(t, pool, hackathonID, projectID, &other, 912, "accepted", "bystander")
	apFxVerdictAs(t, pool, hackathonID, projectID, &other, 913, "accepted", "bystander")

	apFxPublishResults(t, pool, hackathonID, 1)

	appealID, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: appealVerdict, UserID: appellant, Reason: "this was substantial"})
	if err != nil {
		t.Fatalf("SubmitAppeal: %v", err)
	}
	reviewer := fxUser(t, pool)
	if err := DecideAppeal(ctx, pool, AppealDecision{
		AppealID: appealID, ReviewerID: reviewer, Upheld: true, NewBucket: "substantial",
		Reason: "reworked the retry path, not just a config change",
	}); err != nil {
		t.Fatalf("DecideAppeal: %v", err)
	}

	plan, err := CloseAppealsAndRecompute(ctx, pool, hackathonID, reviewer)
	if err != nil {
		t.Fatalf("CloseAppealsAndRecompute: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a recomputed plan")
	}

	// The appellant's single substantial PR is 3 units at full weight. The
	// bystander's three accepted PRs run down the curve: 1.0 + 0.8 + 0.6.
	// 3 + 2.4 = 5.4 effective units.
	if plan.TotalUnits < 5.39 || plan.TotalUnits > 5.41 {
		t.Fatalf("total_units = %v, want 5.4 (substantial at full weight + three accepted down the curve)", plan.TotalUnits)
	}

	// The other three contributors' payouts must have moved too. That is the
	// whole point: before the appeal they were $250 each.
	var amount float64
	if err := pool.QueryRow(ctx, `
SELECT payout_amount FROM hackathon_verdicts WHERE hackathon_id = $1 AND pr_number = 911
`, hackathonID).Scan(&amount); err != nil {
		t.Fatalf("read bystander payout: %v", err)
	}
	if amount > 200 {
		t.Errorf("a bystander's payout is %v, still at or near the pre-appeal $250 - the pool was not redivided", amount)
	}

	// And the pool still sums to what was advertised.
	var total float64
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(SUM(payout_amount), 0) FROM hackathon_verdicts WHERE hackathon_id = $1
`, hackathonID).Scan(&total); err != nil {
		t.Fatalf("sum payouts: %v", err)
	}
	if total < 999 || total > 1001 {
		t.Errorf("payouts sum to %v, want ~1000 (the advertised pool)", total)
	}

	// Exactly once. A second call must not divide the pool again.
	again, err := CloseAppealsAndRecompute(ctx, pool, hackathonID, reviewer)
	if err != nil {
		t.Fatalf("second CloseAppealsAndRecompute: %v", err)
	}
	if again != nil {
		t.Error("the recompute ran twice; it must be idempotent on appeals_closed_at")
	}
	var runs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM hackathon_payout_runs WHERE hackathon_id = $1 AND trigger = 'appeal_recompute'`,
		hackathonID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("appeal_recompute payout runs = %d, want exactly 1", runs)
	}
}

func TestCloseAppealsAndRecompute_RefusesWhileAppealsArePending(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	apFxSetPool(t, pool, hackathonID, 500)

	owner := fxUser(t, pool)
	verdictID := apFxVerdict(t, pool, hackathonID, projectID, &owner, 920, "accepted")
	apFxPublishResults(t, pool, hackathonID, 1)
	if _, err := SubmitAppeal(ctx, pool, SubmitAppealInput{VerdictID: verdictID, UserID: owner, Reason: "undecided"}); err != nil {
		t.Fatalf("SubmitAppeal: %v", err)
	}

	if _, err := CloseAppealsAndRecompute(ctx, pool, hackathonID, owner); err == nil {
		t.Fatal("expected the recompute to refuse while an appeal is still pending")
	}
}

// Money must not move until the event has settled AND the recompute has run.
func TestGuardPayoutRelease_RequiresSettledPhaseAndAClosedAppealWindow(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, _, _ := fxLiveHackathon(t, pool)
	actor := fxUser(t, pool)

	// Leave shadow mode so the phase/appeal conditions are what is under test.
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by)
VALUES ($1, 'judging_shadow_mode', 'false', $2)
`, hackathonID, actor); err != nil {
		t.Fatalf("leave shadow mode: %v", err)
	}

	req := PayoutReleaseRequest{
		HackathonID: hackathonID,
		PayoutRunID: uuid.New(),
		ActorID:     actor,
		Confirm:     true,
	}

	if err := GuardPayoutRelease(ctx, pool, req); !errors.Is(err, ErrPayoutNotReleasable) {
		t.Fatalf("release while live: err = %v, want ErrPayoutNotReleasable", err)
	}

	// Settled, but the appeal window was never closed out - so the §13-#4
	// recompute never ran and the stored unit_value may be stale.
	if _, err := pool.Exec(ctx, `UPDATE hackathons SET phase = 'settled' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("force settled: %v", err)
	}
	err := GuardPayoutRelease(ctx, pool, req)
	if !errors.Is(err, ErrPayoutNotReleasable) {
		t.Fatalf("release with an unclosed appeal window: err = %v, want ErrPayoutNotReleasable", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE hackathons SET appeals_closed_at = now() WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("close appeals: %v", err)
	}
	if err := GuardPayoutRelease(ctx, pool, req); err != nil {
		t.Fatalf("settled with a closed appeal window should release: %v", err)
	}
}
