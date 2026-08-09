package syncjobs

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func TestReconcileApplicationStatuses(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	fullName := fmt.Sprintf("octo/reconcile-test-%s", uuid.New().String()[:8])
	var projectID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified')
RETURNING id
`, ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	insertApplication := func(t *testing.T, issueNumber int, login, status string) {
		t.Helper()
		var appUserID uuid.UUID
		if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&appUserID); err != nil {
			t.Fatalf("insert applicant user: %v", err)
		}
		if _, err := d.Pool.Exec(ctx, `
INSERT INTO issue_applications (user_id, project_id, issue_number, github_login, status, applied_at, assigned_at)
VALUES ($1, $2, $3, $4, $5, now(), CASE WHEN $5 = 'assigned' THEN now() ELSE NULL END)
`, appUserID, projectID, issueNumber, login, status); err != nil {
			t.Fatalf("insert application: %v", err)
		}
	}

	statusFor := func(t *testing.T, issueNumber int, login string) (status string, assignedAtSet bool) {
		t.Helper()
		var assignedAt *string
		if err := d.Pool.QueryRow(ctx, `
SELECT status, assigned_at::text FROM issue_applications
WHERE project_id = $1 AND issue_number = $2 AND LOWER(github_login) = LOWER($3)
`, projectID, issueNumber, login).Scan(&status, &assignedAt); err != nil {
			t.Fatalf("query application status: %v", err)
		}
		return status, assignedAt != nil
	}

	t.Run("eligible applied assignee is promoted to assigned", func(t *testing.T) {
		insertApplication(t, 1, "promote-me", "applied")

		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 1, []string{"promote-me"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 0 {
			t.Errorf("ineligible = %v, want empty", ineligible)
		}
		status, assignedAtSet := statusFor(t, 1, "promote-me")
		if status != "assigned" {
			t.Errorf("status = %q, want assigned", status)
		}
		if !assignedAtSet {
			t.Error("assigned_at should be set after promotion")
		}
	})

	t.Run("assignee with no application is reported ineligible and not written", func(t *testing.T) {
		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 2, []string{"no-such-applicant"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 1 || ineligible[0] != "no-such-applicant" {
			t.Errorf("ineligible = %v, want [no-such-applicant]", ineligible)
		}

		var count int
		if err := d.Pool.QueryRow(ctx, `
SELECT count(*) FROM issue_applications WHERE project_id = $1 AND issue_number = 2
`, projectID).Scan(&count); err != nil {
			t.Fatalf("count applications: %v", err)
		}
		if count != 0 {
			t.Errorf("count = %d, want 0 (no row should be created for an ineligible login)", count)
		}
	})

	t.Run("rejected applicant is reported ineligible", func(t *testing.T) {
		insertApplication(t, 3, "rejected-applicant", "rejected")

		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 3, []string{"rejected-applicant"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 1 || ineligible[0] != "rejected-applicant" {
			t.Errorf("ineligible = %v, want [rejected-applicant]", ineligible)
		}
		status, _ := statusFor(t, 3, "rejected-applicant")
		if status != "rejected" {
			t.Errorf("status = %q, want rejected (unchanged)", status)
		}
	})

	t.Run("assigned row demoted back to applied when GitHub no longer lists them", func(t *testing.T) {
		insertApplication(t, 4, "unassigned-elsewhere", "assigned")

		// GitHub's current assignee list no longer includes this login at all.
		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 4, []string{"someone-else-entirely"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 1 || ineligible[0] != "someone-else-entirely" {
			t.Errorf("ineligible = %v, want [someone-else-entirely]", ineligible)
		}
		status, assignedAtSet := statusFor(t, 4, "unassigned-elsewhere")
		if status != "applied" {
			t.Errorf("status = %q, want applied (demoted)", status)
		}
		if assignedAtSet {
			t.Error("assigned_at should be cleared after demotion")
		}
	})

	t.Run("empty ghAssigneeLogins demotes an existing assigned row", func(t *testing.T) {
		// This is the maintainer-unassigned-directly-on-GitHub case: GitHub
		// reports zero assignees. Confirms ANY($1::text[]) against an empty
		// array behaves as "matches nothing" (always false), not as a no-op
		// the way an empty SQL IN (...) list would be.
		insertApplication(t, 5, "fully-unassigned", "assigned")

		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 5, []string{})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 0 {
			t.Errorf("ineligible = %v, want empty", ineligible)
		}
		status, assignedAtSet := statusFor(t, 5, "fully-unassigned")
		if status != "applied" {
			t.Errorf("status = %q, want applied (demoted even with an empty GitHub assignee list)", status)
		}
		if assignedAtSet {
			t.Error("assigned_at should be cleared after demotion")
		}
	})

	t.Run("already-assigned eligible row is left alone, not re-promoted redundantly", func(t *testing.T) {
		insertApplication(t, 6, "already-assigned", "assigned")

		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 6, []string{"already-assigned"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 0 {
			t.Errorf("ineligible = %v, want empty", ineligible)
		}
		status, assignedAtSet := statusFor(t, 6, "already-assigned")
		if status != "assigned" {
			t.Errorf("status = %q, want assigned", status)
		}
		if !assignedAtSet {
			t.Error("assigned_at should remain set")
		}
	})

	t.Run("case-insensitive login matching", func(t *testing.T) {
		insertApplication(t, 7, "MixedCaseLogin", "applied")

		ineligible, err := reconcileApplicationStatuses(ctx, d.Pool, projectID, 7, []string{"mixedcaselogin"})
		if err != nil {
			t.Fatalf("reconcileApplicationStatuses: %v", err)
		}
		if len(ineligible) != 0 {
			t.Errorf("ineligible = %v, want empty (case-insensitive match)", ineligible)
		}
		status, _ := statusFor(t, 7, "MixedCaseLogin")
		if status != "assigned" {
			t.Errorf("status = %q, want assigned", status)
		}
	})
}

func TestShouldEnforceAssignmentEligibility(t *testing.T) {
	type label = struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}

	cases := []struct {
		name   string
		state  string
		labels []label
		want   bool
	}{
		{
			name:   "open with GrainHack label",
			state:  "open",
			labels: []label{{Name: "GrainHack"}},
			want:   true,
		},
		{
			name:   "open with GrainHack label in a different case",
			state:  "OPEN",
			labels: []label{{Name: "grainhack"}},
			want:   true,
		},
		{
			name:   "closed with GrainHack label is never enforced",
			state:  "closed",
			labels: []label{{Name: "GrainHack"}},
			want:   false,
		},
		{
			name:   "open without GrainHack label is never enforced",
			state:  "open",
			labels: []label{{Name: "bug"}, {Name: "good first issue"}},
			want:   false,
		},
		{
			name:   "open with no labels at all",
			state:  "open",
			labels: nil,
			want:   false,
		},
		{
			name:   "closed with no labels",
			state:  "closed",
			labels: nil,
			want:   false,
		},
		{
			name:   "GrainHack label present among several others",
			state:  "open",
			labels: []label{{Name: "bug"}, {Name: "GrainHack"}, {Name: "priority:high"}},
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldEnforceAssignmentEligibility(tc.state, tc.labels, nil); got != tc.want {
				t.Errorf("shouldEnforceAssignmentEligibility(%q, %v, nil) = %v, want %v", tc.state, tc.labels, got, tc.want)
			}
		})
	}
}
