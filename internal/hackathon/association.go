package hackathon

import (
	"context"
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
	// SharedOrg is true when the applicant is a member of the issue's org.
	// The org-member hard gate (§4.1) already blocks that case, so this
	// stays false in practice - it exists so the signal is still complete
	// if an admin ever disables block_org_members.
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
