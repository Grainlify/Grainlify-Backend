package handlers

// Unit tests for the five recordX write helpers (recordApplication,
// recordAssignment, recordRejection, recordWithdrawal, recordUnassignment),
// called directly against a real test DB rather than through Apply/Assign/
// Reject/Withdraw/Unassign's HTTP handlers. This is deliberate, not a
// shortcut: every one of those five HTTP handlers calls the real GitHub API
// before it ever reaches its recordX call, and issue_applications_test.go's
// own file-level doc comment explains that path isn't reachable in tests
// without live GitHub credentials this suite doesn't have. Testing the
// helpers directly is the only way to get real coverage of this persistence
// logic.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// issueAppsInternalCreateProject seeds a minimal projects row. Package-local
// copy of projectsFxInsertProject's shape: an internal (package handlers)
// test file cannot call a helper defined in the external handlers_test
// package, even though both live in this directory - see referralsTestDB's
// own doc comment for the same reasoning.
func issueAppsInternalCreateProject(t *testing.T, d *db.DB, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified')
RETURNING id
`, ownerID, "issue-apps-internal-test-"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("issueAppsInternalCreateProject: insert: %v", err)
	}
	return id
}

type issueAppsInternalRow struct {
	Status      string
	AppliedAt   *time.Time
	AssignedAt  *time.Time
	ResolvedAt  *time.Time
	GitHubLogin string
}

func issueAppsInternalGetRow(t *testing.T, d *db.DB, projectID uuid.UUID, issueNumber int, userID uuid.UUID) issueAppsInternalRow {
	t.Helper()
	var row issueAppsInternalRow
	err := d.Pool.QueryRow(context.Background(), `
SELECT status, applied_at, assigned_at, resolved_at, github_login
FROM issue_applications
WHERE project_id = $1 AND issue_number = $2 AND user_id = $3
`, projectID, issueNumber, userID).Scan(&row.Status, &row.AppliedAt, &row.AssignedAt, &row.ResolvedAt, &row.GitHubLogin)
	if err != nil {
		t.Fatalf("issueAppsInternalGetRow: %v", err)
	}
	return row
}

func TestRecordApplication_InsertsThenUpsertsOnReapply(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	projectID := issueAppsInternalCreateProject(t, d, userID)
	commentID := int64(12345)

	if err := recordApplication(context.Background(), d.Pool, userID, projectID, 1, "alice", &commentID); err != nil {
		t.Fatalf("recordApplication: %v", err)
	}
	row := issueAppsInternalGetRow(t, d, projectID, 1, userID)
	if row.Status != "applied" || row.AppliedAt == nil || row.GitHubLogin != "alice" {
		t.Fatalf("after apply: status=%q appliedAt=%v login=%q", row.Status, row.AppliedAt, row.GitHubLogin)
	}

	// Re-applying after a withdrawal should flip status back to 'applied'
	// (the ON CONFLICT path), not error on the duplicate (project, issue,
	// user) key.
	if err := recordWithdrawal(context.Background(), d.Pool, userID, projectID, 1); err != nil {
		t.Fatalf("recordWithdrawal: %v", err)
	}
	if err := recordApplication(context.Background(), d.Pool, userID, projectID, 1, "alice", &commentID); err != nil {
		t.Fatalf("recordApplication (reapply): %v", err)
	}
	row = issueAppsInternalGetRow(t, d, projectID, 1, userID)
	if row.Status != "applied" {
		t.Fatalf("after reapply: status = %q, want applied", row.Status)
	}
}

func TestRecordAssignment_UpsertsWithoutPriorApplication(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	projectID := issueAppsInternalCreateProject(t, d, userID)

	// A maintainer can Assign() a contributor who never called Apply() -
	// recordAssignment must create the row from scratch, with applied_at
	// left nil since there was no organic "applied" event.
	if err := recordAssignment(context.Background(), d.Pool, userID, projectID, 2, "bob"); err != nil {
		t.Fatalf("recordAssignment: %v", err)
	}
	row := issueAppsInternalGetRow(t, d, projectID, 2, userID)
	if row.Status != "assigned" || row.AssignedAt == nil {
		t.Fatalf("after direct assign: status=%q assignedAt=%v", row.Status, row.AssignedAt)
	}
	if row.AppliedAt != nil {
		t.Fatalf("after direct assign: applied_at = %v, want nil (no prior Apply())", row.AppliedAt)
	}
}

func TestRecordUnassignment_RevertsToAppliedAndClearsAssignedAt(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	projectID := issueAppsInternalCreateProject(t, d, userID)
	commentID := int64(999)

	if err := recordApplication(context.Background(), d.Pool, userID, projectID, 3, "carol", &commentID); err != nil {
		t.Fatalf("recordApplication: %v", err)
	}
	if err := recordAssignment(context.Background(), d.Pool, userID, projectID, 3, "carol"); err != nil {
		t.Fatalf("recordAssignment: %v", err)
	}
	row := issueAppsInternalGetRow(t, d, projectID, 3, userID)
	if row.Status != "assigned" || row.AssignedAt == nil {
		t.Fatalf("after assign: status=%q assignedAt=%v", row.Status, row.AssignedAt)
	}

	if err := recordUnassignment(context.Background(), d.Pool, projectID, 3); err != nil {
		t.Fatalf("recordUnassignment: %v", err)
	}
	row = issueAppsInternalGetRow(t, d, projectID, 3, userID)
	if row.Status != "applied" {
		t.Fatalf("after unassign: status = %q, want applied", row.Status)
	}
	if row.AssignedAt != nil {
		t.Fatalf("after unassign: assigned_at = %v, want nil (cleared)", row.AssignedAt)
	}
	if row.AppliedAt == nil {
		t.Fatalf("after unassign: applied_at should still be set from the original Apply()")
	}
}

func TestRecordRejection_UpdatesOnlyExistingApplicationCaseInsensitive(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	projectID := issueAppsInternalCreateProject(t, d, userID)
	commentID := int64(1)

	// Rejecting a login with no application on file has nothing to persist -
	// must not error.
	if err := recordRejection(context.Background(), d.Pool, projectID, 4, "nobody"); err != nil {
		t.Fatalf("recordRejection (no-op case): %v", err)
	}

	if err := recordApplication(context.Background(), d.Pool, userID, projectID, 4, "DAVE", &commentID); err != nil {
		t.Fatalf("recordApplication: %v", err)
	}
	// GitHub logins are case-insensitive; recordRejection matches accordingly.
	if err := recordRejection(context.Background(), d.Pool, projectID, 4, "dave"); err != nil {
		t.Fatalf("recordRejection: %v", err)
	}
	row := issueAppsInternalGetRow(t, d, projectID, 4, userID)
	if row.Status != "rejected" || row.ResolvedAt == nil {
		t.Fatalf("after reject: status=%q resolvedAt=%v", row.Status, row.ResolvedAt)
	}
}

func TestRecordWithdrawal_MarksCallersOwnApplication(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)
	projectID := issueAppsInternalCreateProject(t, d, userID)
	commentID := int64(1)

	if err := recordApplication(context.Background(), d.Pool, userID, projectID, 5, "erin", &commentID); err != nil {
		t.Fatalf("recordApplication: %v", err)
	}
	if err := recordWithdrawal(context.Background(), d.Pool, userID, projectID, 5); err != nil {
		t.Fatalf("recordWithdrawal: %v", err)
	}
	row := issueAppsInternalGetRow(t, d, projectID, 5, userID)
	if row.Status != "withdrawn" || row.ResolvedAt == nil {
		t.Fatalf("after withdraw: status=%q resolvedAt=%v", row.Status, row.ResolvedAt)
	}
}

func TestIssueLabelNames(t *testing.T) {
	got := issueLabelNames([]byte(`[{"name":"bug","color":"d73a4a"},{"name":"good first issue","color":"7057ff"}]`))
	if len(got) != 2 || got[0] != "bug" || got[1] != "good first issue" {
		t.Fatalf("issueLabelNames = %v, want [bug, good first issue]", got)
	}

	if got := issueLabelNames([]byte(`[]`)); len(got) != 0 {
		t.Fatalf("issueLabelNames([]) = %v, want empty", got)
	}

	if got := issueLabelNames(nil); len(got) != 0 {
		t.Fatalf("issueLabelNames(nil) = %v, want empty (not a panic)", got)
	}
}

func TestIsGitHubInstallationNotFoundError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"real GitHub 404 shape", fmt.Errorf("failed to get installation token: status 404, error: map[message:Not Found status:404]"), true},
		{"mixed-case Not Found", fmt.Errorf("some wrapper: Installation Not Found"), true},
		{"network timeout", fmt.Errorf("context deadline exceeded"), false},
		{"401 auth error", fmt.Errorf("status 401, error: map[message:Bad credentials]"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGitHubInstallationNotFoundError(tc.err); got != tc.want {
				t.Errorf("isGitHubInstallationNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
