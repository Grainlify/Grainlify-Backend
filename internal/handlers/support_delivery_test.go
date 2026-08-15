package handlers

import (
	"testing"
	"time"
)

// "Delivered" is category-dependent, and there are two copies of that rule:
// supportTelegramDelivered in Go, and the predicate in the partial index.
// Two copies is how the leaderboard and the profile came to disagree about the
// same contributor, so these tests exist to make the copies provably equal
// rather than hopefully equal.

func ptr(t time.Time) *time.Time { return &t }

func TestSupportTelegramDelivered_KYCCountsOnlyTheAdminDM(t *testing.T) {
	now := time.Now()

	// The dangerous combination the separate columns exist for: something was
	// posted, but the details reached nobody. For KYC there is no public post
	// at all, so a stamped topic column can only be wrong - and it must never
	// read as delivered.
	if supportTelegramDelivered("kyc", ptr(now), nil) {
		t.Error("kyc with only a topic post must NOT count as delivered - the details reached nobody")
	}
	if !supportTelegramDelivered("kyc", nil, ptr(now)) {
		t.Error("kyc with the admin DM delivered must count as delivered")
	}
	if supportTelegramDelivered("kyc", nil, nil) {
		t.Error("kyc with neither must not count as delivered")
	}
}

func TestSupportTelegramDelivered_OtherCategoriesCountTheTopicPost(t *testing.T) {
	now := time.Now()
	for _, cat := range []string{"bug", "idea", "help", "other"} {
		if !supportTelegramDelivered(cat, ptr(now), nil) {
			t.Errorf("%s with a topic post must count as delivered", cat)
		}
		// These never receive a DM, so a stamped DM column cannot substitute.
		if supportTelegramDelivered(cat, nil, ptr(now)) {
			t.Errorf("%s must not count as delivered on the admin DM column", cat)
		}
		if supportTelegramDelivered(cat, nil, nil) {
			t.Errorf("%s with neither must not count as delivered", cat)
		}
	}
}

func TestSupportFullyDelivered_RequiresBothSinks(t *testing.T) {
	now := time.Now()
	if supportFullyDelivered("bug", nil, ptr(now), nil) {
		t.Error("a bug delivered to telegram but not discord is not fully delivered")
	}
	if supportFullyDelivered("kyc", ptr(now), nil, nil) {
		t.Error("a kyc delivered to discord but not the admin DM is not fully delivered")
	}
	if !supportFullyDelivered("kyc", ptr(now), nil, ptr(now)) {
		t.Error("kyc with discord and the admin DM is fully delivered")
	}
	if !supportFullyDelivered("bug", ptr(now), ptr(now), nil) {
		t.Error("bug with discord and the topic post is fully delivered")
	}
}

// The predicate must not be one that every row satisfies. The version this
// replaced was `discord IS NULL OR telegram IS NULL OR admin_dm IS NULL`,
// under which every row matched forever - each category leaves one of the two
// telegram columns NULL by design - so a replay job built on it would have
// redelivered everything on every run.
func TestSupportUndeliveredPredicate_IsNotAlwaysTrue(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name                    string
		category                string
		discord, topic, adminDM *time.Time
		wantUndelivered         bool
	}{
		{"bug fully delivered", "bug", ptr(now), ptr(now), nil, false},
		{"kyc fully delivered", "kyc", ptr(now), nil, ptr(now), false},
		{"bug missing telegram", "bug", ptr(now), nil, nil, true},
		{"kyc missing admin dm", "kyc", ptr(now), nil, nil, true},
		{"kyc with only a topic post", "kyc", ptr(now), ptr(now), nil, true},
		{"nothing delivered", "other", nil, nil, nil, true},
	}
	sawDelivered := false
	for _, tc := range cases {
		got := !supportFullyDelivered(tc.category, tc.discord, tc.topic, tc.adminDM)
		if got != tc.wantUndelivered {
			t.Errorf("%s: undelivered = %v, want %v", tc.name, got, tc.wantUndelivered)
		}
		if !got {
			sawDelivered = true
		}
	}
	if !sawDelivered {
		t.Fatal("no combination counted as delivered - the rule is unsatisfiable, which is the bug this test exists for")
	}
}
