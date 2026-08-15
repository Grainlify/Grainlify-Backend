package handlers

import (
	"testing"
	"time"
)

// "Delivered" is category-dependent, and there are two copies of that rule:
// the supportDelivered functions in Go, and the predicate in the partial index.
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

// KYC is never sent to Discord, so discord_delivered_at stays NULL for ever on
// those rows. Without this branch every KYC row would look permanently
// undelivered and a replay would retry a send that is never going to happen.
func TestSupportDiscordDelivered_KYCIsNeverSentSoNothingIsPending(t *testing.T) {
	now := time.Now()

	if !supportDiscordDelivered("kyc", nil) {
		t.Error("kyc with a NULL discord column must not read as pending - it is never sent there")
	}
	// True regardless of the timestamp, because the question is "is anything
	// outstanding", not "was something posted".
	if !supportDiscordDelivered("kyc", ptr(now)) {
		t.Error("kyc must read as having nothing outstanding for discord")
	}

	for _, cat := range []string{"bug", "idea", "help", "other"} {
		if supportDiscordDelivered(cat, nil) {
			t.Errorf("%s with a NULL discord column IS outstanding", cat)
		}
		if !supportDiscordDelivered(cat, ptr(now)) {
			t.Errorf("%s with a stamped discord column is delivered", cat)
		}
	}
}

func TestSupportFullyDelivered_RequiresEveryRouteTheCategoryUses(t *testing.T) {
	now := time.Now()
	if supportFullyDelivered("bug", nil, ptr(now), nil) {
		t.Error("a bug delivered to telegram but not discord is not fully delivered")
	}
	if !supportFullyDelivered("bug", ptr(now), ptr(now), nil) {
		t.Error("bug with discord and the topic post is fully delivered")
	}

	// The change that matters: a KYC request is complete on the admin DM
	// alone. Requiring discord too would have left every one of them pending
	// for ever, since it is deliberately never sent there.
	if !supportFullyDelivered("kyc", nil, nil, ptr(now)) {
		t.Error("kyc with the admin DM is fully delivered - discord is never part of its route")
	}
	if supportFullyDelivered("kyc", ptr(now), nil, nil) {
		t.Error("a kyc without the admin DM is not fully delivered, whatever else is stamped")
	}
	if supportFullyDelivered("kyc", ptr(now), ptr(now), nil) {
		t.Error("a kyc with a topic post but no DM reached nobody who can act on it")
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
		{"kyc delivered, nothing else stamped", "kyc", nil, nil, ptr(now), false},
		{"bug missing telegram", "bug", ptr(now), nil, nil, true},
		{"bug missing discord", "bug", nil, ptr(now), nil, true},
		{"kyc missing admin dm", "kyc", nil, nil, nil, true},
		{"kyc with only a topic post", "kyc", nil, ptr(now), nil, true},
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
