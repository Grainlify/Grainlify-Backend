package handlers

import "testing"

// The states in which a user may open a new KYC session.
//
// Regression: "not_started" was treated as an active session and blocked, so a
// user who clicked verify once and closed the tab could never start again
// without an admin deleting the session by hand. It was reported from
// production, where an account sat at not_started with a session id and every
// attempt returned 409 kyc_session_exists.
func TestCanStartNewKYCSession(t *testing.T) {
	ptr := func(s string) *string { return &s }

	for _, tc := range []struct {
		name   string
		status *string
		want   bool
		why    string
	}{
		{"no session ever", nil, true, "nothing to protect"},
		{"empty string", ptr(""), true, "same as absent"},
		{"expired", ptr("expired"), true, "session was deleted in the Didit dashboard"},
		{"not started", ptr("not_started"), true, "link created, never opened - no progress to lose"},
		{"pending", ptr("pending"), false, "verification is genuinely in flight"},
		{"in review", ptr("in_review"), false, "submitted, awaiting a decision"},
		{"verified", ptr("verified"), false, "already done"},
		{"rejected", ptr("rejected"), false, "has a result; reopening needs an admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canStartNewKYCSession(tc.status); got != tc.want {
				t.Errorf("canStartNewKYCSession(%v) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
			}
		})
	}
}
