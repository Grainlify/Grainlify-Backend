package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func noPrimaryLanguage() (string, error) { return "", nil }

func TestSyncIssueLabel_NoAcceptedApplication_NoOp(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)

	err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 1, []string{"GrainHack"}, true, noPrimaryLanguage)
	if err != nil {
		t.Fatalf("SyncIssueLabel: %v", err)
	}

	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM hackathon_issues WHERE project_id = $1`, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no hackathon_issues row without an accepted application, got %d", count)
	}
}

func TestSyncIssueLabel_LabelPresent_CreatesPendingRow(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "acme-org")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 42, []string{"grainhack"}, true, noPrimaryLanguage)
	if err != nil {
		t.Fatalf("SyncIssueLabel: %v", err)
	}

	var status, orgLogin string
	if err := d.Pool.QueryRow(ctx, `
SELECT status, org_login FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 42
`, hackathonID, projectID).Scan(&status, &orgLogin); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if orgLogin != "acme-org" {
		t.Errorf("org_login = %q, want acme-org", orgLogin)
	}
}

func TestSyncIssueLabel_AutoPublishesOnceBothFieldsSet(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 7, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel: %v", err)
	}
	var issueID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `SELECT id FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 7`, hackathonID, projectID).Scan(&issueID); err != nil {
		t.Fatalf("query id: %v", err)
	}

	// Not yet published: only one of the two required fields set.
	if _, err := d.Pool.Exec(ctx, `UPDATE hackathon_issues SET acceptance_criteria = 'must pass tests' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := MaybePublish(ctx, d.Pool, hackathonID, issueID); err != nil {
		t.Fatalf("MaybePublish (1st field only): %v", err)
	}
	var status string
	if err := d.Pool.QueryRow(ctx, `SELECT status FROM hackathon_issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want still pending (only 1 of 2 required fields set)", status)
	}

	// Now set the second field - should auto-publish.
	if _, err := d.Pool.Exec(ctx, `UPDATE hackathon_issues SET difficulty_tier = 'easy' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := MaybePublish(ctx, d.Pool, hackathonID, issueID); err != nil {
		t.Fatalf("MaybePublish (both fields): %v", err)
	}
	var publishedAt *time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT status, published_at FROM hackathon_issues WHERE id = $1`, issueID).Scan(&status, &publishedAt); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "published" {
		t.Errorf("status = %q, want published", status)
	}
	if publishedAt == nil {
		t.Error("published_at is nil, want a real timestamp")
	}
}

func TestSyncIssueLabel_LabelRemoved_MarksRemoved(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 9, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (create): %v", err)
	}
	// Label no longer present.
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 9, []string{"bug"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (remove): %v", err)
	}

	var status string
	var removedAt *time.Time
	if err := d.Pool.QueryRow(ctx, `
SELECT status, removed_at FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 9
`, hackathonID, projectID).Scan(&status, &removedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "removed" {
		t.Errorf("status = %q, want removed", status)
	}
	if removedAt == nil {
		t.Error("removed_at is nil, want a real timestamp")
	}
}

func TestSyncIssueLabel_ReEntryAfterRemoval_RestoresSurvivingFields(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 3, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (create): %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
UPDATE hackathon_issues SET acceptance_criteria = 'crit', difficulty_tier = 'easy'
WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 3
`, hackathonID, projectID); err != nil {
		t.Fatalf("set fields: %v", err)
	}
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 3, []string{"bug"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (remove): %v", err)
	}

	// Re-add the label - since both fields survived removal, this should
	// restore straight to published, not pending.
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 3, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (re-add): %v", err)
	}

	var status, acceptanceCriteria, difficultyTier string
	var removedAt *time.Time
	if err := d.Pool.QueryRow(ctx, `
SELECT status, COALESCE(acceptance_criteria,''), COALESCE(difficulty_tier,''), removed_at
FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 3
`, hackathonID, projectID).Scan(&status, &acceptanceCriteria, &difficultyTier, &removedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "published" {
		t.Errorf("status = %q, want published (fields survived removal)", status)
	}
	if acceptanceCriteria != "crit" || difficultyTier != "easy" {
		t.Errorf("fields after restore = (%q, %q), want (crit, easy) - must survive removal", acceptanceCriteria, difficultyTier)
	}
	if removedAt != nil {
		t.Error("removed_at should be cleared after re-entry")
	}
}

func TestSyncIssueLabel_PerOrgCap(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	admin := fxAdmin(t, d.Pool)
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "capped-org")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	// Override the cap to a small number so this test doesn't need 50 fixture rows.
	if err := SetValue(ctx, d.Pool, &hackathonID, "max_issues_per_org", "2", admin); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, i, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
			t.Fatalf("SyncIssueLabel (issue %d): %v", i, err)
		}
	}
	// The 3rd issue exceeds the cap - must not be inserted, and must not error.
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 3, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (over cap): %v", err)
	}

	var count int
	if err := d.Pool.QueryRow(ctx, `
SELECT COUNT(*) FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND status != 'removed'
`, hackathonID, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want exactly 2 (the cap) - the 3rd over-cap issue must not be inserted", count)
	}
	var thirdExists bool
	if err := d.Pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = 3)
`, hackathonID, projectID).Scan(&thirdExists); err != nil {
		t.Fatalf("exists check: %v", err)
	}
	if thirdExists {
		t.Error("issue 3 should not have any row at all (rejected before insert), not even a removed one")
	}
}

func TestSyncIssueLabel_ClosedIssue_NoFreshInsert(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "issue_prep"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)

	// isOpen=false on a never-before-seen issue - must not create a row.
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 55, []string{"GrainHack"}, false, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel: %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM hackathon_issues WHERE project_id = $1 AND issue_number = 55`, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no row for a closed issue with no prior entry, got %d", count)
	}
}

func TestSyncIssueLabel_LateEntry_RespectsAllowLateAndCutoff(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	admin := fxAdmin(t, d.Pool)
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")
	fullName := fxProjectFullName(t, d.Pool, projectID)

	// live phase, ends in 1 hour, default cutoff is 48h - past the cutoff.
	hackathonID := fxHackathon(t, d.Pool, fxHackathonSpec{Phase: "live"})
	fxAcceptedApplication(t, d.Pool, hackathonID, projectID, owner)
	if _, err := d.Pool.Exec(ctx, `UPDATE hackathons SET ends_at = $1 WHERE id = $2`, time.Now().Add(1*time.Hour), hackathonID); err != nil {
		t.Fatalf("update ends_at: %v", err)
	}

	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 1, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (past cutoff): %v", err)
	}
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM hackathon_issues WHERE project_id = $1 AND issue_number = 1`, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no row - within the default 48h late-entry cutoff of a 1h-away end date, got %d", count)
	}

	// Shrink the cutoff so this same hackathon now accepts late entries.
	if err := SetValue(ctx, d.Pool, &hackathonID, "late_entry_cutoff_hours", "0", admin); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 2, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (within cutoff): %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM hackathon_issues WHERE project_id = $1 AND issue_number = 2`, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected a row once the cutoff shrinks to 0h, got count=%d", count)
	}

	// Disabling late entry entirely blocks it regardless of cutoff.
	if err := SetValue(ctx, d.Pool, &hackathonID, "allow_late_issue_entry", "false", admin); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if err := SyncIssueLabel(ctx, d.Pool, nil, nil, nil, projectID, fullName, 4, []string{"GrainHack"}, true, noPrimaryLanguage); err != nil {
		t.Fatalf("SyncIssueLabel (late entry disabled): %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM hackathon_issues WHERE project_id = $1 AND issue_number = 4`, projectID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no row with allow_late_issue_entry=false, got %d", count)
	}
}
