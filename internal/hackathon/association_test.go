package hackathon

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// SharedOrg was declared, scored and never assigned - dead weight in the
// score. It now reads from data we hold: does the applicant own a project
// under the same org?
func TestComputePriorAssociation_SharedOrgIsActuallyPopulated(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	var fullName string
	if err := pool.QueryRow(ctx, `SELECT github_full_name FROM projects WHERE id = $1`, projectID).Scan(&fullName); err != nil {
		t.Fatalf("read project: %v", err)
	}
	orgLogin := strings.SplitN(fullName, "/", 2)[0]

	outsider := fxUser(t, pool)
	pa := ComputePriorAssociation(ctx, pool, hackathonID, outsider, "outsider", orgLogin)
	if pa.SharedOrg {
		t.Error("an unrelated applicant was marked as sharing the org")
	}

	// Someone who owns a repo in the same org.
	insider := fxUser(t, pool)
	fxProject(t, pool, insider, orgLogin+"/insider-repo-"+uuid.New().String()[:8])
	pa = ComputePriorAssociation(ctx, pool, hackathonID, insider, "insider", orgLogin)
	if !pa.SharedOrg {
		t.Error("an applicant owning a repo in the same org was not marked as sharing it")
	}
	if pa.Score < 3 {
		t.Errorf("score = %d, want the shared-org signal to contribute", pa.Score)
	}
}

// The rollup is advisory. §4.2 never blocks and §7 makes eligibility
// reductions admin-reviewable, so crossing the threshold must change nothing
// on its own.
func TestOrgAssociationSummaries_FlagsWithoutBlocking(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	var orgLogin string
	if err := pool.QueryRow(ctx,
		`SELECT SPLIT_PART(github_full_name, '/', 1) FROM projects WHERE id = $1`, projectID).Scan(&orgLogin); err != nil {
		t.Fatalf("org: %v", err)
	}

	for i := 0; i < 3; i++ {
		issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 1400+i, "standard")
		u := fxUser(t, pool)
		asg := fxAssignment(t, pool, hackathonID, issueID, projectID, u, 1400+i, "repeat", nil)
		if _, err := pool.Exec(ctx, `
UPDATE hackathon_assignments
SET prior_association = '{"score":4,"frequent_merge_relation":true,"shared_org":false,"prior_grainhack_co_occurrence":2}'::jsonb
WHERE id = $1`, asg); err != nil {
			t.Fatalf("set association: %v", err)
		}
	}

	summaries, err := OrgAssociationSummaries(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("OrgAssociationSummaries: %v", err)
	}
	var found *OrgAssociationSummary
	for i := range summaries {
		if strings.EqualFold(summaries[i].OrgLogin, orgLogin) {
			found = &summaries[i]
		}
	}
	if found == nil {
		t.Fatalf("org %q missing from summaries", orgLogin)
	}
	if found.WithEvidence != 3 {
		t.Errorf("with_evidence = %d, want 3", found.WithEvidence)
	}
	if found.FrequentMerge != 3 {
		t.Errorf("frequent_merge = %d, want 3", found.FrequentMerge)
	}
	if !found.Flagged {
		t.Error("three associated assignments did not reach the advisory flag")
	}

	// Advisory only: the assignments themselves are untouched.
	var active int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int FROM hackathon_assignments
WHERE hackathon_id = $1 AND status = 'active'`, hackathonID).Scan(&active); err != nil {
		t.Fatalf("count: %v", err)
	}
	if active != 3 {
		t.Errorf("active assignments = %d, want 3 untouched - flagging must not block", active)
	}
}
