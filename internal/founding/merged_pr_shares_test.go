package founding

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// hackathonAndProject creates the minimum an event needs for a verdict to
// exist against it.
func hackathonAndProject(t *testing.T, d *db.DB) (hackathonID, projectID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := newUser(t, d)
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO hackathons (name, phase) VALUES ('founding-test', 'closed') RETURNING id
`).Scan(&hackathonID); err != nil {
		t.Fatalf("insert hackathon: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified') RETURNING id
`, owner, "founding/"+uuid.NewString()[:8]).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return hackathonID, projectID
}

// mergedPRFixture creates a hackathon, project and verdict, optionally with a
// merged pull request behind it, and returns the verdict id.
func mergedPRFixture(t *testing.T, d *db.DB, hackathonID, projectID, userID uuid.UUID, prNumber int, bucket string, merged bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	mergedAt := "NULL"
	if merged {
		mergedAt = "now()"
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, merged_at_github)
VALUES ($1, $2, $3, `+mergedAt+`)
`, projectID, int64(uuid.New().ID()), prNumber); err != nil {
		t.Fatalf("insert pull request: %v", err)
	}

	var verdictID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket)
VALUES ($1, $2, $3, $4, 'tester', NULLIF($5, ''))
RETURNING id
`, hackathonID, projectID, prNumber, userID, bucket).Scan(&verdictID); err != nil {
		t.Fatalf("insert verdict: %v", err)
	}
	return verdictID
}

// TestGrantMergedPRShares_RequiresBothMergeAndAcceptedVerdict is the rule
// itself. Neither condition alone is sound: a merge is a signal a maintainer
// controls by themselves, and a verdict is not final until appeals close.
func TestGrantMergedPRShares_RequiresBothMergeAndAcceptedVerdict(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	hackathonID, projectID := hackathonAndProject(t, d)

	merged := newUser(t, d)
	mergedRejected := newUser(t, d)
	unmergedAccepted := newUser(t, d)
	unjudged := newUser(t, d)

	mergedPRFixture(t, d, hackathonID, projectID, merged, 1, "accepted", true)
	mergedPRFixture(t, d, hackathonID, projectID, mergedRejected, 2, "rejected", true)
	mergedPRFixture(t, d, hackathonID, projectID, unmergedAccepted, 3, "accepted", false)
	mergedPRFixture(t, d, hackathonID, projectID, unjudged, 4, "", true)

	if _, err := GrantMergedPRShares(ctx, d.Pool, hackathonID, cfg); err != nil {
		t.Fatalf("GrantMergedPRShares: %v", err)
	}

	for _, tc := range []struct {
		name string
		user uuid.UUID
		want float64
	}{
		{"merged and accepted", merged, 5},
		{"merged but rejected - a maintainer merging is not enough", mergedRejected, 0},
		{"accepted but never merged", unmergedAccepted, 0},
		{"merged but no final verdict yet", unjudged, 0},
	} {
		got, err := TotalFor(ctx, d.Pool, tc.user)
		if err != nil {
			t.Fatalf("TotalFor(%s): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: shares = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGrantMergedPRShares_ReRunIsANoOp: the recompute can fire more than once,
// and this runs after its commit as a best-effort step, so re-running has to
// be safe rather than merely unlikely.
func TestGrantMergedPRShares_ReRunIsANoOp(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	hackathonID, projectID := hackathonAndProject(t, d)
	u := newUser(t, d)
	mergedPRFixture(t, d, hackathonID, projectID, u, 10, "exceptional", true)

	for i := 0; i < 3; i++ {
		if _, err := GrantMergedPRShares(ctx, d.Pool, hackathonID, cfg); err != nil {
			t.Fatalf("GrantMergedPRShares run %d: %v", i, err)
		}
	}
	got, err := TotalFor(ctx, d.Pool, u)
	if err != nil {
		t.Fatalf("TotalFor: %v", err)
	}
	if got != 5 {
		t.Errorf("shares after three runs = %v, want 5 - a re-run must not pay again", got)
	}
}

// TestGrantMergedPRShares_AlsoPaysTheReferrer covers the uncapped side of the
// referral rules: bringing somebody who ships is the behaviour worth paying
// for without limit, because it cannot be faked cheaply.
func TestGrantMergedPRShares_AlsoPaysTheReferrer(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	hackathonID, projectID := hackathonAndProject(t, d)
	referrer := newUser(t, d)
	shipper := newUser(t, d)
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO referrals (referrer_user_id, referred_user_id, status)
VALUES ($1, $2, 'completed')
`, referrer, shipper); err != nil {
		t.Fatalf("insert referral: %v", err)
	}

	mergedPRFixture(t, d, hackathonID, projectID, shipper, 20, "accepted", true)
	if _, err := GrantMergedPRShares(ctx, d.Pool, hackathonID, cfg); err != nil {
		t.Fatalf("GrantMergedPRShares: %v", err)
	}

	b, err := BreakdownFor(ctx, d.Pool, referrer)
	if err != nil {
		t.Fatalf("BreakdownFor: %v", err)
	}
	if b.ReferralMergedPR != 5 {
		t.Errorf("referrer referral-merged-PR shares = %v, want 5", b.ReferralMergedPR)
	}
}
