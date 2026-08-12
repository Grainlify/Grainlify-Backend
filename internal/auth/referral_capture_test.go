package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

const capSecret = "referral-capture-test-secret"

// TestReferralCapture_RoundTrips - the ordinary case has to work, or the
// window is just an outage.
func TestReferralCapture_RoundTrips(t *testing.T) {
	tok, err := IssueReferralCapture(capSecret, "ABC123", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("IssueReferralCapture: %v", err)
	}
	code, err := ParseReferralCapture(capSecret, tok)
	if err != nil {
		t.Fatalf("ParseReferralCapture: %v", err)
	}
	if code != "ABC123" {
		t.Errorf("code = %q, want ABC123", code)
	}
}

// TestReferralCapture_ExpiredTokenIsRejected is the rule the whole change
// exists for: a stale click cannot be presented as a fresh one.
func TestReferralCapture_ExpiredTokenIsRejected(t *testing.T) {
	// Issued with a TTL that has already elapsed by the time it is parsed.
	tok, err := IssueReferralCapture(capSecret, "OLDCODE", time.Nanosecond)
	if err != nil {
		t.Fatalf("IssueReferralCapture: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := ParseReferralCapture(capSecret, tok); err == nil {
		t.Error("an expired capture token was accepted")
	}
}

// TestReferralCapture_TamperingIsRejected. The client holds this token, so
// the only thing stopping it from rewriting the code or the timestamp is the
// signature.
func TestReferralCapture_TamperingIsRejected(t *testing.T) {
	tok, _ := IssueReferralCapture(capSecret, "REAL", time.Hour)

	// Wrong key entirely.
	if _, err := ParseReferralCapture("a-different-secret", tok); err == nil {
		t.Error("a token signed with another key was accepted")
	}
	// Body edited, signature left alone.
	if len(tok) > 10 {
		mangled := tok[:len(tok)-6] + "AAAAAA"
		if _, err := ParseReferralCapture(capSecret, mangled); err == nil {
			t.Error("a token with a mangled signature was accepted")
		}
	}
	// Not a token at all - what a hand-rolled bypass attempt looks like.
	if _, err := ParseReferralCapture(capSecret, "ABC123"); err == nil {
		t.Error("a bare referral code was accepted as a capture token")
	}
}

// TestReferralCapture_RejectsATokenIssuedForSomethingElse. A session JWT and a
// capture token are both HS256 with the same secret, so the only thing
// separating them is the subject - without this check a session token would
// parse here and carry no code.
func TestReferralCapture_RejectsATokenIssuedForSomethingElse(t *testing.T) {
	session, err := IssueJWT(capSecret, uuid.New(), "admin", "", "", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}
	if _, err := ParseReferralCapture(capSecret, session); err == nil {
		t.Error("a session token was accepted as a referral capture token")
	}
}

// TestReferralCapture_RefusesEmptyInputs.
func TestReferralCapture_RefusesEmptyInputs(t *testing.T) {
	if _, err := IssueReferralCapture("", "ABC", time.Hour); err == nil {
		t.Error("issued a token with no secret")
	}
	if _, err := IssueReferralCapture(capSecret, "   ", time.Hour); err == nil {
		t.Error("issued a token for a blank code")
	}
	if _, err := IssueReferralCapture(capSecret, "ABC", 0); err == nil {
		t.Error("issued a token with no expiry")
	}
}
