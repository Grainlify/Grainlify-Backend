package hackathon

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// clFxRated seeds one assignment and rates it, returning the assignment id.
func clFxRated(t *testing.T, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber, rating int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, issueNumber, "standard")
	user := fxUser(t, pool)
	asg := fxAssignment(t, pool, hackathonID, issueID, projectID, user, issueNumber, "rater", nil)
	if err := SubmitClarityRating(ctx, pool, asg, user, rating, ""); err != nil {
		t.Fatalf("clFxRated: %v", err)
	}
	return asg
}

// The rating is optional throughout. Nothing gates PR submission on it, and a
// contributor who never rates simply has no row - the maintainer's aggregate
// is computed from whoever chose to answer.
func TestSubmitClarityRating_IsOptionalAndOwnedByTheAssignee(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 900, "standard")

	owner := fxUser(t, pool)
	stranger := fxUser(t, pool)
	asg := fxAssignment(t, pool, hackathonID, issueID, projectID, owner, 900, "owner", nil)

	// Someone else's assignment is not theirs to rate.
	if err := SubmitClarityRating(ctx, pool, asg, stranger, 5, ""); !errors.Is(err, ErrClarityNotYourAssignment) {
		t.Fatalf("stranger rating: err = %v, want ErrClarityNotYourAssignment", err)
	}

	// Out-of-range is refused rather than clamped: a 7 is a bug in the
	// caller, and silently storing 5 would hide it.
	if err := SubmitClarityRating(ctx, pool, asg, owner, 7, ""); err == nil {
		t.Error("expected a rating of 7 to be refused")
	}

	if err := SubmitClarityRating(ctx, pool, asg, owner, 4, "issue was clear enough"); err != nil {
		t.Fatalf("SubmitClarityRating: %v", err)
	}

	// Correcting a misclick replaces the answer rather than adding a second.
	if err := SubmitClarityRating(ctx, pool, asg, owner, 2, "actually it was ambiguous"); err != nil {
		t.Fatalf("re-rate: %v", err)
	}
	count, mean, err := ClarityScore(ctx, pool, hackathonID, projectID)
	if err != nil {
		t.Fatalf("ClarityScore: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d after re-rating, want 1", count)
	}
	if mean != 2 {
		t.Errorf("mean = %v, want 2 (the corrected answer)", mean)
	}
}

// The maintainer must not see ratings arriving live: timing alone would let
// them work out who said what, and those people still need PRs merged.
func TestClarityForMaintainer_HiddenUntilTheEventCloses(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	for i := 0; i < 4; i++ {
		clFxRated(t, pool, hackathonID, projectID, 910+i, 4)
	}

	agg, err := ClarityForMaintainer(ctx, pool, hackathonID, projectID)
	if err != nil {
		t.Fatalf("ClarityForMaintainer: %v", err)
	}
	if agg.Visible {
		t.Error("ratings were visible to the maintainer while the event was still live")
	}
	if agg.Count != 0 || agg.Mean != 0 {
		t.Errorf("a hidden aggregate leaked numbers: count=%d mean=%v", agg.Count, agg.Mean)
	}
	if agg.Reason == "" {
		t.Error("no reason given; the UI would render an unexplained empty panel")
	}

	if _, err := pool.Exec(ctx, `UPDATE hackathons SET phase = 'closed' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("close event: %v", err)
	}
	agg, err = ClarityForMaintainer(ctx, pool, hackathonID, projectID)
	if err != nil {
		t.Fatalf("ClarityForMaintainer (closed): %v", err)
	}
	if !agg.Visible || agg.Count != 4 {
		t.Errorf("after close: visible=%v count=%d, want visible with 4", agg.Visible, agg.Count)
	}
}

// Below the floor the "aggregate" is just one person's answer read back.
func TestClarityForMaintainer_WithheldBelowTheAnonymityFloor(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	clFxRated(t, pool, hackathonID, projectID, 920, 1)
	if _, err := pool.Exec(ctx, `UPDATE hackathons SET phase = 'closed' WHERE id = $1`, hackathonID); err != nil {
		t.Fatalf("close event: %v", err)
	}

	agg, err := ClarityForMaintainer(ctx, pool, hackathonID, projectID)
	if err != nil {
		t.Fatalf("ClarityForMaintainer: %v", err)
	}
	if agg.Visible {
		t.Error("a single rating was shown as an aggregate; the maintainer can read that person's answer directly")
	}
	if agg.Mean != 0 {
		t.Errorf("mean leaked as %v below the floor", agg.Mean)
	}
}

// The anti-gaming property is the timing, so it is verified rather than
// assumed: a rating stamped at or after results publication is excluded from
// the score even if it somehow got written.
func TestClarityScore_ExcludesRatingsSubmittedAfterResultsWerePublished(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	clFxRated(t, pool, hackathonID, projectID, 930, 5)
	clFxRated(t, pool, hackathonID, projectID, 931, 5)
	late := clFxRated(t, pool, hackathonID, projectID, 932, 1)

	// Publish results, then force the third rating's timestamp after it - the
	// shape a replayed request or a backfill would produce.
	if _, err := pool.Exec(ctx, `
UPDATE hackathons SET phase = 'results_published', results_published_at = now() WHERE id = $1
`, hackathonID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issue_clarity_ratings SET submitted_at = now() + interval '1 hour'
WHERE assignment_id = $1
`, late); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	count, mean, err := ClarityScore(ctx, pool, hackathonID, projectID)
	if err != nil {
		t.Fatalf("ClarityScore: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 - the post-publication rating must be excluded", count)
	}
	if mean != 5 {
		t.Errorf("mean = %v, want 5; the excluded 1-star rating still moved the score", mean)
	}

	// And the write path refuses it outright once results are out.
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 933, "standard")
	user := fxUser(t, pool)
	asg := fxAssignment(t, pool, hackathonID, issueID, projectID, user, 933, "latecomer", nil)
	if err := SubmitClarityRating(ctx, pool, asg, user, 5, ""); !errors.Is(err, ErrClarityWindowClosed) {
		t.Errorf("late submit: err = %v, want ErrClarityWindowClosed", err)
	}
}
