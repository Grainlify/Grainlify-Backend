package hackathon

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// PriorAssociation is AI-specs.md §4.2's collusion signal between an
// applicant and the maintainer/org whose issue they applied to.
//
// **This never blocks.** It is recorded on the application and assignment,
// surfaced in admin review, and feeds maintainer-pool eligibility. §4.2 is
// explicit about why: "Blocking would punish legitimate repeat contributors
// - exactly the relationship the platform wants to grow."
type PriorAssociation struct {
	// SharedOrg is true when the applicant already holds a project under the
	// issue's org on Grainlify - the strongest shared-org evidence available
	// without a GitHub call.
	//
	// A deliberate proxy for §4.2's "shared org membership, current or past".
	// True membership needs an authenticated org call per applicant, and the
	// org-member hard gate (§4.1) already blocks members outright, so this
	// only matters when an admin has disabled block_org_members - at which
	// point a proxy computed from data we hold beats a signal that is never
	// populated at all. It was previously declared, scored, and never
	// assigned, which made it dead weight in the score.
	SharedOrg bool `json:"shared_org"`
	// MergedPRsByMaintainer counts this applicant's PRs already merged into
	// the org's repos. §4.2's threshold is 3.
	MergedPRsByMaintainer int  `json:"merged_prs_by_maintainer"`
	FrequentMergeRelation bool `json:"frequent_merge_relation"`
	// PriorGrainHackCoOccurrence counts earlier hackathons where this
	// applicant was assigned an issue from the same org.
	PriorGrainHackCoOccurrence int `json:"prior_grainhack_co_occurrence"`
	// AccountsCreatedWithin7Days compares the applicant's GitHub account
	// age to the project owner's. Nil when either creation date is unknown
	// - an unknown is not evidence.
	AccountsCreatedWithin7Days *bool `json:"accounts_created_within_7_days"`
	// Score is a convenience roll-up for admin sorting, not a threshold.
	Score int `json:"score"`
}

// ComputePriorAssociation gathers §4.2's signals from data we already hold.
// Best-effort per field, matching the rest of this package: a failure on one
// signal leaves it at its zero value rather than failing the application,
// because this is evidence for a human, not a gate.
func ComputePriorAssociation(ctx context.Context, pool db.DBPool, hackathonID, userID uuid.UUID, githubLogin, orgLogin string) *PriorAssociation {
	pa := &PriorAssociation{}

	// PRs by this applicant already merged into any repo in the org.
	_ = pool.QueryRow(ctx, `
SELECT count(*)
FROM github_pull_requests pr
JOIN projects p ON p.id = pr.project_id
WHERE pr.merged
  AND lower(pr.author_login) = lower($1)
  AND lower(SPLIT_PART(p.github_full_name, '/', 1)) = lower($2)
`, githubLogin, orgLogin).Scan(&pa.MergedPRsByMaintainer)
	pa.FrequentMergeRelation = pa.MergedPRsByMaintainer >= 3

	// Shared org: does this applicant own a project under the same org?
	_ = pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM projects
  WHERE owner_user_id = $1
    AND lower(SPLIT_PART(github_full_name, '/', 1)) = lower($2)
)
`, userID, orgLogin).Scan(&pa.SharedOrg)

	// Assignments this applicant already won from the same org in *earlier*
	// hackathons - repeated co-occurrence across events.
	_ = pool.QueryRow(ctx, `
SELECT count(*)
FROM hackathon_assignments
WHERE user_id = $1 AND lower(org_login) = lower($2) AND hackathon_id <> $3
`, userID, orgLogin, hackathonID).Scan(&pa.PriorGrainHackCoOccurrence)

	// Account-creation proximity to the org's project owner. Uses the
	// github_accounts row's own created_at (when they linked Grainlify),
	// which is a weaker proxy than GitHub's account age but needs no API
	// call; the AI-specs signal is "created within 7 days of each other".
	var applicantCreated, ownerCreated *time.Time
	_ = pool.QueryRow(ctx, `SELECT created_at FROM github_accounts WHERE user_id = $1`, userID).Scan(&applicantCreated)
	_ = pool.QueryRow(ctx, `
SELECT ga.created_at
FROM projects p
JOIN github_accounts ga ON ga.user_id = p.owner_user_id
WHERE lower(SPLIT_PART(p.github_full_name, '/', 1)) = lower($1)
ORDER BY ga.created_at ASC
LIMIT 1
`, orgLogin).Scan(&ownerCreated)
	if applicantCreated != nil && ownerCreated != nil {
		diff := applicantCreated.Sub(*ownerCreated)
		if diff < 0 {
			diff = -diff
		}
		within := diff <= 7*24*time.Hour
		pa.AccountsCreatedWithin7Days = &within
	}

	if pa.SharedOrg {
		pa.Score += 3
	}
	if pa.FrequentMergeRelation {
		pa.Score += 2
	}
	if pa.PriorGrainHackCoOccurrence > 0 {
		pa.Score += pa.PriorGrainHackCoOccurrence
	}
	if pa.AccountsCreatedWithin7Days != nil && *pa.AccountsCreatedWithin7Days {
		pa.Score += 2
	}
	return pa
}

// OrgAssociationSummary aggregates §4.2 evidence for one org, for the
// maintainer-pool eligibility review §7 calls for.
type OrgAssociationSummary struct {
	OrgLogin string `json:"org_login"`
	// Assignments with any association evidence at all, and the subset with
	// enough to be worth a human look.
	Assignments      int `json:"assignments"`
	WithEvidence     int `json:"with_evidence"`
	SharedOrg        int `json:"shared_org"`
	FrequentMerge    int `json:"frequent_merge_relation"`
	RepeatCoOccurred int `json:"repeat_co_occurrence"`
	CloseAccountAges int `json:"accounts_created_within_7_days"`
	// Flagged is advisory. §4.2 is explicit that this never blocks, and §7
	// makes eligibility reductions "admin-reviewable, not automatic" - the
	// same reasoning as the out-of-band assignment threshold. A maintainer
	// whose repeat contributors keep winning issues is describing the
	// relationship the platform exists to grow; the same numbers also
	// describe collusion, and only a human can tell those apart.
	Flagged   bool `json:"flagged"`
	Threshold int  `json:"threshold"`
}

// OrgAssociationSummaries rolls up the prior_association evidence already
// stored on each assignment.
//
// Reads the snapshot written at draw time rather than recomputing. The
// evidence that mattered is what was true when the assignment was made, and
// recomputing months later against a repo that has moved on would answer a
// different question than the one being reviewed.
func OrgAssociationSummaries(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) ([]OrgAssociationSummary, error) {
	threshold := 3

	rows, err := pool.Query(ctx, `
SELECT org_login,
       count(*)::int,
       count(*) FILTER (WHERE prior_association IS NOT NULL
                          AND (prior_association->>'score')::numeric > 0)::int,
       count(*) FILTER (WHERE (prior_association->>'shared_org')::boolean)::int,
       count(*) FILTER (WHERE (prior_association->>'frequent_merge_relation')::boolean)::int,
       count(*) FILTER (WHERE COALESCE((prior_association->>'prior_grainhack_co_occurrence')::int, 0) > 0)::int,
       count(*) FILTER (WHERE (prior_association->>'accounts_created_within_7_days')::boolean)::int
FROM hackathon_assignments
WHERE hackathon_id = $1
GROUP BY org_login
ORDER BY 3 DESC, org_login
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.OrgAssociationSummaries: %w", err)
	}
	defer rows.Close()

	out := []OrgAssociationSummary{}
	for rows.Next() {
		var s OrgAssociationSummary
		if err := rows.Scan(&s.OrgLogin, &s.Assignments, &s.WithEvidence, &s.SharedOrg,
			&s.FrequentMerge, &s.RepeatCoOccurred, &s.CloseAccountAges); err != nil {
			return nil, fmt.Errorf("hackathon.OrgAssociationSummaries: scan: %w", err)
		}
		s.Threshold = threshold
		s.Flagged = s.WithEvidence >= threshold
		out = append(out, s)
	}
	return out, rows.Err()
}
