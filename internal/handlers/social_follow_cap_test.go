package handlers

import (
	"os"
	"strings"
	"testing"
)

// The approval cap, and the one thing that makes it a cap.
//
// The constant is checked INSIDE applyDecision - in the same transaction as
// the write, after a transaction-scoped advisory lock. That placement is the
// whole property: checked in the handler before the transaction, two admins
// approving simultaneously both read 299, both pass, and both commit, so the
// cap is exceeded by however many people are working at once. It would look
// correct in every test that ran one request at a time.
//
// Asserted against the source because the race cannot be provoked reliably
// from a single-process test, and because what is being pinned is the
// PLACEMENT rather than an observable output - a check in the wrong place
// returns the right answer nearly always.
func TestApprovalCap_IsCheckedInsideTheTransactionUnderALock(t *testing.T) {
	src := readSourceFile(t, "social_follow.go")

	body := sliceBetween(t, src,
		"func (h *SocialFollowHandler) applyDecision(",
		"\n// socialFollowCanTransition")

	for _, want := range []string{
		"pg_advisory_xact_lock",
		"social_follow_approval_cap",
		"WHERE status = 'approved'",
		"errSocialFollowCapReached",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("applyDecision does not contain %q - the cap must be enforced inside the transaction, "+
				"under the lock, or two concurrent approvals both pass", want)
		}
	}

	// And the count must be read AFTER the lock is taken, not before it.
	lockAt := strings.Index(body, "pg_advisory_xact_lock")
	countAt := strings.Index(body, "SELECT count(*)::int FROM social_follow_submissions")
	if lockAt < 0 || countAt < 0 || countAt < lockAt {
		t.Error("the approved count is read before the advisory lock is taken, which is the same race unlocked")
	}
}

// The bulk path must not have its own copy of the rule. It gets per-row
// enforcement by calling applyDecision per row; a single check before the loop
// would admit a whole batch against the last remaining slot.
func TestApprovalCap_BulkReliesOnThePerRowPath(t *testing.T) {
	src := readSourceFile(t, "social_follow.go")
	bulk := sliceBetween(t, src, "func (h *SocialFollowHandler) BulkApprove(", "\n// ")

	if !strings.Contains(bulk, "h.applyDecision(c.Context(), id,") {
		t.Error("BulkApprove no longer goes through applyDecision per row - the cap would not apply per row")
	}
	if strings.Contains(bulk, "socialFollowApprovalCap") {
		t.Error("BulkApprove has its own copy of the cap check; one definition, in applyDecision, is the point")
	}
	if !strings.Contains(bulk, "cap_reached") {
		t.Error("BulkApprove does not report cap_reached per row, so a skipped batch looks like a successful one")
	}
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// sliceBetween returns the source between two markers, failing loudly if
// either is absent - the case that would otherwise make every assertion below
// it pass against an empty string.
func sliceBetween(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("could not find %q; if it was renamed, update this test rather than deleting it", from)
	}
	rest := src[i:]
	if j := strings.Index(rest, to); j > 0 {
		return rest[:j]
	}
	return rest
}
