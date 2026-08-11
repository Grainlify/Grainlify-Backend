package founding

import (
	"context"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// TestEligible_RevokedApprovalIsNotEligible is the path most likely to break
// silently.
//
// "Never approved" and "approved, then revoked" both have to come out as not
// eligible, but only the first is obvious. A revoked submission still looks
// like a submission: the row exists, both screenshots are there, and it was
// genuinely approved at one point. Any check that asks "is there a
// submission?" or "was this ever approved?" passes it - which is exactly what
// the old social_follow_completions table did, since a row there meant "was
// paid" rather than "is eligible now".
//
// It also has to be distinguishable in the *reason*, not just the verdict.
// Somebody whose eligibility was withdrawn is owed a different explanation
// from somebody who never submitted.
func TestEligible_RevokedApprovalIsNotEligible(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	cfg := defaults()
	cfg["founding_require_social_follow"] = "true"

	user := newUser(t, d)

	// Approved: eligible.
	socialFollowWithStatus(t, d, user, "approved")
	ok, reason, err := Eligible(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("Eligible(approved): %v", err)
	}
	if !ok || reason != "" {
		t.Fatalf("approved: eligible = %v, reason = %q; want true with no reason", ok, reason)
	}

	// Same row, approval withdrawn: not eligible.
	socialFollowWithStatus(t, d, user, "revoked")
	ok, reason, err = Eligible(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("Eligible(revoked): %v", err)
	}
	if ok {
		t.Error("a revoked approval is still conferring eligibility")
	}
	if reason == "" {
		t.Error("no reason recorded for an ineligible revoked submission")
	}

	// And the reason must say which of the two happened.
	never := newUser(t, d)
	_, neverReason, err := Eligible(ctx, d.Pool, never, cfg)
	if err != nil {
		t.Fatalf("Eligible(no submission): %v", err)
	}
	if neverReason == reason {
		t.Errorf("a revoked approval and a missing submission give the same reason (%q); "+
			"they are different situations and the person is owed different explanations", reason)
	}
}

// TestEligible_PendingAndRejectedAreNotEligible rounds out the states.
func TestEligible_PendingAndRejectedAreNotEligible(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	cfg := defaults()
	cfg["founding_require_social_follow"] = "true"

	for _, status := range []string{"pending", "rejected"} {
		user := newUser(t, d)
		socialFollowWithStatus(t, d, user, status)
		ok, reason, err := Eligible(ctx, d.Pool, user, cfg)
		if err != nil {
			t.Fatalf("Eligible(%s): %v", status, err)
		}
		if ok {
			t.Errorf("%s submission is conferring eligibility", status)
		}
		if reason == "" {
			t.Errorf("%s: no reason recorded", status)
		}
	}
}

// TestEligible_GateCanBeTurnedOff: with the requirement disabled nobody is
// held back by it, submission or not.
func TestEligible_GateCanBeTurnedOff(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	cfg := defaults()
	cfg["founding_require_social_follow"] = "false"

	user := newUser(t, d)
	socialFollowWithStatus(t, d, user, "revoked")
	ok, _, err := Eligible(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("Eligible: %v", err)
	}
	if !ok {
		t.Error("the social-follow gate is still applying when it is turned off")
	}
}
