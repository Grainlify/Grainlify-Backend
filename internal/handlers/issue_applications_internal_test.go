package handlers

import "testing"

// TestAssigneeWasApplied is the regression test for issue #293: GitHub's
// AddIssueAssignees returns a 2xx and silently omits a login from the
// resulting assignees array when it isn't a valid assignee, rather than
// erroring. assigneeWasApplied is what Assign() uses to detect that
// omission instead of treating a nil error as confirmation.
func TestAssigneeWasApplied(t *testing.T) {
	tests := []struct {
		name    string
		applied []string
		login   string
		want    bool
	}{
		{name: "exact match", applied: []string{"alice", "bob"}, login: "bob", want: true},
		{name: "case-insensitive match", applied: []string{"Alice"}, login: "alice", want: true},
		{name: "silently dropped by github", applied: []string{"existing-assignee"}, login: "no-access-user", want: false},
		{name: "empty applied list", applied: nil, login: "alice", want: false},
		{name: "similar but distinct login not matched", applied: []string{"alice2"}, login: "alice", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := assigneeWasApplied(tc.applied, tc.login); got != tc.want {
				t.Errorf("assigneeWasApplied(%v, %q) = %v, want %v", tc.applied, tc.login, got, tc.want)
			}
		})
	}
}
