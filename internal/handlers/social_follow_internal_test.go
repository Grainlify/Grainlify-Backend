package handlers

import (
	"context"
	"testing"
)

func TestIsValidSocialFollowPlatform(t *testing.T) {
	for _, p := range []string{"github", "telegram", "linkedin"} {
		if !isValidSocialFollowPlatform(p) {
			t.Errorf("isValidSocialFollowPlatform(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"twitter", "facebook", "", "GITHUB"} {
		if isValidSocialFollowPlatform(p) {
			t.Errorf("isValidSocialFollowPlatform(%q) = true, want false", p)
		}
	}
}

func TestMaybeCompleteSocialFollow_RequiresAllPlatformsApproved(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	// Only 2 of 3 platforms approved.
	for _, platform := range []string{"github", "telegram"} {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO social_follow_submissions (user_id, platform, screenshot, status)
VALUES ($1, $2, 'data:image/png;base64,x', 'approved')
`, userID, platform); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}

	maybeCompleteSocialFollow(context.Background(), d, nil, userID)

	var count int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM social_follow_completions WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("completions count = %d, want 0 with only 2/3 platforms approved", count)
	}
}

func TestMaybeCompleteSocialFollow_AwardsOnceAllThreeApproved(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	for _, platform := range socialFollowPlatforms {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO social_follow_submissions (user_id, platform, screenshot, status)
VALUES ($1, $2, 'data:image/png;base64,x', 'approved')
`, userID, platform); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}

	maybeCompleteSocialFollow(context.Background(), d, nil, userID)

	var pointsAwarded int
	if err := d.Pool.QueryRow(context.Background(), `
SELECT points_awarded FROM social_follow_completions WHERE user_id = $1
`, userID).Scan(&pointsAwarded); err != nil {
		t.Fatalf("expected a completion row: %v", err)
	}
	if pointsAwarded != socialFollowPointsPerCompletion {
		t.Errorf("points_awarded = %d, want %d", pointsAwarded, socialFollowPointsPerCompletion)
	}

	var ledgerAmount int
	if err := d.Pool.QueryRow(context.Background(), `
SELECT amount FROM point_ledger WHERE user_id = $1 AND reason = 'social_follow'
`, userID).Scan(&ledgerAmount); err != nil {
		t.Fatalf("expected a ledger entry: %v", err)
	}
	if ledgerAmount != socialFollowPointsPerCompletion {
		t.Errorf("ledger amount = %d, want %d", ledgerAmount, socialFollowPointsPerCompletion)
	}
}

func TestMaybeCompleteSocialFollow_SecondCallDoesNotDoubleAward(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	for _, platform := range socialFollowPlatforms {
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO social_follow_submissions (user_id, platform, screenshot, status)
VALUES ($1, $2, 'data:image/png;base64,x', 'approved')
`, userID, platform); err != nil {
			t.Fatalf("seed submission: %v", err)
		}
	}

	maybeCompleteSocialFollow(context.Background(), d, nil, userID)
	maybeCompleteSocialFollow(context.Background(), d, nil, userID) // e.g. re-approving after an edit

	var ledgerEntries int
	if err := d.Pool.QueryRow(context.Background(), `
SELECT count(*) FROM point_ledger WHERE user_id = $1 AND reason = 'social_follow'
`, userID).Scan(&ledgerEntries); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if ledgerEntries != 1 {
		t.Errorf("social_follow ledger entries = %d, want exactly 1 (must not double-award)", ledgerEntries)
	}
}
