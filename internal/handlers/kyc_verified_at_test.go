package handlers_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// kyc_verified_at answers "when did this person verify". It must be written
// once, on the transition, and never moved again.
//
// It used to be `CASE WHEN $1 = 'verified' THEN now()` in both the webhook and
// the status poll, so every redelivery and every poll re-dated somebody who
// was already verified. The poll runs whenever the contributor opens their
// billing page, so the date drifted forward for as long as anyone kept
// looking.
//
// The damage was invisible until it was checked against something independent:
// all 37 founding members had a kyc_verified_at LATER than the wave assignment
// that their verification caused - impossible - by up to 26 hours. Sequence
// order matched assigned_at 37/37 and kyc_verified_at 4/37, which is how we
// know which column was lying.
//
// These exercise the SQL directly. Both call sites run the same CASE
// expression, and the branch that matters is decided by Postgres, not by Go.

// applyKYCDecision runs the same write the webhook and the poll both use.
func applyKYCDecision(t *testing.T, userID uuid.UUID, status string) {
	t.Helper()
	d := testDB(t)
	if _, err := d.Pool.Exec(t.Context(), `
UPDATE users
SET kyc_status = $1,
    kyc_verified_at = CASE
      WHEN $1 = 'verified' AND kyc_status IS DISTINCT FROM 'verified' THEN now()
      ELSE kyc_verified_at
    END,
    updated_at = now()
WHERE id = $2
`, status, userID); err != nil {
		t.Fatalf("apply %q: %v", status, err)
	}
}

func kycVerifiedAtOf(t *testing.T, userID uuid.UUID) *time.Time {
	t.Helper()
	d := testDB(t)
	var at *time.Time
	if err := d.Pool.QueryRow(t.Context(), `SELECT kyc_verified_at FROM users WHERE id = $1`, userID).Scan(&at); err != nil {
		t.Fatalf("read kyc_verified_at: %v", err)
	}
	return at
}

func TestKYCVerifiedAt_StampedOnceAndNeverMoved(t *testing.T) {
	d := testDB(t)
	userID := leaderboardSuiteUser(t, d.Pool)

	applyKYCDecision(t, userID, "pending")
	if at := kycVerifiedAtOf(t, userID); at != nil {
		t.Fatalf("kyc_verified_at set while merely pending: %v", at)
	}

	applyKYCDecision(t, userID, "verified")
	first := kycVerifiedAtOf(t, userID)
	if first == nil {
		t.Fatal("kyc_verified_at not stamped on the transition into verified")
	}

	// A redelivered webhook, and a status poll, both observing the same state.
	// This is the exact sequence that corrupted every row in production.
	time.Sleep(15 * time.Millisecond)
	applyKYCDecision(t, userID, "verified")
	time.Sleep(15 * time.Millisecond)
	applyKYCDecision(t, userID, "verified")

	after := kycVerifiedAtOf(t, userID)
	if after == nil {
		t.Fatal("kyc_verified_at was cleared by a repeat observation")
	}
	if !after.Equal(*first) {
		t.Errorf("kyc_verified_at moved on a repeat observation: %v -> %v (drift %v)",
			first, after, after.Sub(*first))
	}
}

// A non-verified decision must never disturb an existing date either.
func TestKYCVerifiedAt_SurvivesALaterNonVerifiedDecision(t *testing.T) {
	d := testDB(t)
	userID := leaderboardSuiteUser(t, d.Pool)

	applyKYCDecision(t, userID, "verified")
	first := kycVerifiedAtOf(t, userID)
	if first == nil {
		t.Fatal("not stamped")
	}

	for _, s := range []string{"in_review", "rejected", "expired"} {
		applyKYCDecision(t, userID, s)
		got := kycVerifiedAtOf(t, userID)
		if got == nil || !got.Equal(*first) {
			t.Errorf("after %q: kyc_verified_at = %v, want it untouched at %v", s, got, first)
		}
	}
}

// Verifying again after a reset IS a real transition and does re-stamp: the
// contributor genuinely verified at that later moment. The rule is "stamp on
// the transition", not "stamp only when null" - those differ exactly here.
func TestKYCVerifiedAt_ReVerifyingAfterAResetDoesRestamp(t *testing.T) {
	d := testDB(t)
	userID := leaderboardSuiteUser(t, d.Pool)

	applyKYCDecision(t, userID, "verified")
	first := kycVerifiedAtOf(t, userID)
	if first == nil {
		t.Fatal("not stamped")
	}

	// A reset leaves kyc_verified_at alone and moves the status away.
	applyKYCDecision(t, userID, "expired")
	time.Sleep(15 * time.Millisecond)
	applyKYCDecision(t, userID, "verified")

	second := kycVerifiedAtOf(t, userID)
	if second == nil {
		t.Fatal("cleared by re-verification")
	}
	if !second.After(*first) {
		t.Errorf("re-verification after a reset did not re-stamp: %v -> %v", first, second)
	}
}

// The tests above run their own copy of the SQL, which is two copies of one
// rule - the shape that has already made the leaderboard and the profile
// disagree about the same contributor, and made a support-delivery predicate
// pass while the migration it mirrored was broken.
//
// So this reads the actual handler sources and asserts that every write of
// kyc_verified_at is guarded by the transition check. It fails if a third
// writer appears, or if either existing one reverts - the case the behavioural
// tests above cannot see.
//
// Precedent: TestUndeliveredIndexMatchesTheGoPredicate reads its migration
// from disk for exactly this reason.
func TestKYCVerifiedAt_EveryWriterGuardsTheTransition(t *testing.T) {
	// Every non-test file in the package, not a hand-written list.
	//
	// The list used to name kyc.go and didit_webhook.go. That made the "a
	// third writer appears" promise above untrue: kyc_status_reconciler.go
	// arrived carrying its own copy of this CASE and was never read by this
	// test, because a file nobody adds to the slice is a file this cannot see.
	// The count stayed at 2 and looked correct throughout.
	//
	// A structural check that enumerates its own inputs by hand can only find
	// what somebody remembered to give it - which is the failure mode this
	// file exists to prevent, applied to itself.
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var files []string
	for _, name := range entries {
		if !strings.HasSuffix(name, "_test.go") {
			files = append(files, name)
		}
	}
	if len(files) < 10 {
		t.Fatalf("globbed only %d source files; the scan is not running where it thinks it is", len(files))
	}

	found := 0
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "kyc_verified_at = CASE") {
				continue
			}
			found++
			// The whole CASE spans several lines; the guard lives on the WHEN.
			// Assert on the surrounding block rather than the single line.
			idx := strings.Index(string(src), line)
			block := string(src)[idx:min(idx+300, len(src))]
			if !strings.Contains(block, "kyc_status IS DISTINCT FROM 'verified'") {
				t.Errorf("%s: a kyc_verified_at write is not guarded by the transition check:\n%s",
					name, block)
			}
		}
	}

	// If this number changes, a writer was added or removed and somebody needs
	// to decide which. A guard that silently covers fewer call sites than it
	// used to is the failure mode this file exists to prevent.
	//
	// Two, and they are now kyc.go and kyc_status_change.go. The webhook and
	// the reconciler no longer carry their own: both call applyKYCStatus, so
	// the rule has one definition rather than three that happen to agree.
	const wantWriters = 2
	if found != wantWriters {
		t.Errorf("found %d kyc_verified_at writers, want %d - a new one needs the same guard, "+
			"and a removed one needs this count updated deliberately", found, wantWriters)
	}
}
