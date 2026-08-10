package hackathon

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AutoRevertOOBAssignment reports whether Grainlify may remove an out-of-band
// assignee from a GitHub issue and comment explaining why.
//
// AI-specs.md §2.3, config key auto_revert_oob_assignment: "Automatically
// remove and comment on GitHub-direct (out-of-band) assignments to GrainHack
// issues."
//
// The gate covers the GitHub write only. It deliberately does *not* gate
// Grainlify's own issue_applications reconciliation: an admin switching this
// off is asking us to stop touching their repository, not asking us to stop
// keeping accurate records. The out-of-band assignment is still counted
// either way (§2.3), which is what makes the switch safe to turn off - the
// evidence survives even when the enforcement does not.
//
// **Fails closed.** Any error - missing config, unreachable database, a
// malformed value - returns false, meaning no comment and no assignee
// removal. The two failure directions are not symmetric: not reverting lapses
// for one sync tick and re-converges on the next, while wrongly reverting
// writes to a repository we do not own and notifies a real contributor that
// they were removed. A posted comment cannot be recalled from the inboxes it
// already reached.
//
// Resolution follows the usual cascade. A project inside an active hackathon
// reads that hackathon's value; a project carrying the literal "GrainHack"
// label without any hackathon (see EffectiveGrainHackLabels, which widens
// protection to those) falls through to the global default, which ships as
// true - so nothing changes for anyone who has not turned this off.
func AutoRevertOOBAssignment(ctx context.Context, pool db.DBPool, projectID uuid.UUID) bool {
	hackathonID, _, _, _, found, err := findActiveAcceptedHackathon(ctx, pool, projectID)
	if err != nil {
		return false
	}

	var scope *uuid.UUID
	if found {
		scope = &hackathonID
	}

	v, err := EffectiveValue(ctx, pool, scope, "auto_revert_oob_assignment")
	if err != nil {
		return false
	}
	return v == "true"
}

// OOBAssignment is one recorded out-of-band assignment.
type OOBAssignment struct {
	ID            uuid.UUID  `json:"id"`
	HackathonID   *uuid.UUID `json:"hackathon_id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	RepoFullName  string     `json:"repo_full_name"`
	OrgLogin      string     `json:"org_login"`
	IssueNumber   int        `json:"issue_number"`
	AssignedLogin string     `json:"assigned_login"`
	MaintainerID  *uuid.UUID `json:"maintainer_user_id"`
	Reverted      bool       `json:"reverted"`
	RevertedAt    *time.Time `json:"reverted_at"`
	Occurrences   int        `json:"occurrences"`
	FirstSeenAt   time.Time  `json:"first_seen_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
}

// RecordOOBAssignment records one out-of-band assignment (AI-specs.md §2.3
// step 3), whether or not the revert fired.
//
// reverted says whether Grainlify actually removed the assignee and
// commented. It is stored per event rather than read back from config,
// because auto_revert_oob_assignment can change between events and "why was
// this one left alone?" is a question an admin will ask months later.
//
// On re-observation, occurrences only increments when the previous sighting
// had been reverted. The reasoning: the sync worker re-reads every issue on
// every run. If the revert fired, the assignee was removed from GitHub, so
// seeing the same login assigned again is a genuinely new assignment and
// counts. If the revert did not fire, the assignee is simply still sitting
// there - the same standing violation, not a new one each tick - so only
// last_seen_at moves. Without that distinction, switching the revert off
// would make the count climb on its own until it crossed the threshold and
// flagged an org that did nothing further.
//
// The org, the responsible maintainer and the hackathon are all resolved here
// from projectID rather than passed in, so a caller cannot accidentally
// attribute an event to the wrong org - the call site is inside a sync loop
// where several similar-looking identifiers are in scope.
func RecordOOBAssignment(
	ctx context.Context,
	pool db.DBPool,
	projectID uuid.UUID,
	issueNumber int,
	assignedLogin string,
	reverted bool,
) error {
	var fullName string
	var maintainerID uuid.UUID
	if err := pool.QueryRow(ctx, `
SELECT github_full_name, owner_user_id FROM projects WHERE id = $1
`, projectID).Scan(&fullName, &maintainerID); err != nil {
		return fmt.Errorf("hackathon.RecordOOBAssignment: load project: %w", err)
	}
	orgLogin := orgLoginFromFullName(fullName)

	// Best effort: a project can be enforced without belonging to a hackathon
	// (EffectiveGrainHackLabels covers the literal label), and the event is
	// still worth recording with a null hackathon_id.
	var hackathonID *uuid.UUID
	if hid, _, _, _, found, err := findActiveAcceptedHackathon(ctx, pool, projectID); err == nil && found {
		hackathonID = &hid
	}

	var revertedAt any
	if reverted {
		revertedAt = time.Now()
	}
	_, err := pool.Exec(ctx, `
INSERT INTO hackathon_oob_assignments
  (hackathon_id, project_id, org_login, issue_number, assigned_login,
   maintainer_user_id, reverted, reverted_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (project_id, issue_number, assigned_login) DO UPDATE SET
  last_seen_at = now(),
  occurrences = hackathon_oob_assignments.occurrences
    + CASE WHEN hackathon_oob_assignments.reverted THEN 1 ELSE 0 END,
  reverted = EXCLUDED.reverted,
  reverted_at = COALESCE(EXCLUDED.reverted_at, hackathon_oob_assignments.reverted_at)
`, hackathonID, projectID, orgLogin, issueNumber, assignedLogin, maintainerID, reverted, revertedAt)
	if err != nil {
		return fmt.Errorf("hackathon.RecordOOBAssignment: %w", err)
	}
	return nil
}

// OOBOrgSummary is one org's out-of-band assignment record.
type OOBOrgSummary struct {
	OrgLogin string `json:"org_login"`
	// Total counts re-assignments, not just distinct issues - three
	// assignments to one issue is the same pattern as one each to three.
	Total int `json:"total"`
	// DistinctAssignees separates "a maintainer who does not understand the
	// rules" from "a maintainer routing issues to one unfamiliar account".
	DistinctAssignees int `json:"distinct_assignees"`
	DistinctIssues    int `json:"distinct_issues"`
	// Flagged means Total has reached oob_assignment_flag_threshold.
	//
	// It flags for admin review and nothing else. It applies no penalty,
	// reduces no eligibility, and withholds no money on its own. Three
	// out-of-band assignments could be a maintainer who has not read the
	// rules or a maintainer routing issues to an alt account, and those look
	// identical from here. §7 makes maintainer-pool eligibility reductions
	// "admin-reviewable, not automatic" for exactly this reason.
	Flagged     bool            `json:"flagged"`
	Threshold   int             `json:"threshold"`
	LastSeenAt  time.Time       `json:"last_seen_at"`
	Assignments []OOBAssignment `json:"assignments,omitempty"`
}

// OOBOrgSummaries returns per-org out-of-band assignment counts for a
// hackathon, most recent first, with the flag computed against the current
// threshold.
//
// The flag is computed on read rather than stored. A stored flag drifts the
// moment an admin changes the threshold, and this one is an input to a
// decision about someone's money.
func OOBOrgSummaries(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) ([]OOBOrgSummary, error) {
	raw, err := EffectiveValue(ctx, pool, &hackathonID, "oob_assignment_flag_threshold")
	if err != nil {
		return nil, fmt.Errorf("hackathon.OOBOrgSummaries: read threshold: %w", err)
	}
	threshold := atoiOr(raw, 3)
	if threshold < 1 {
		threshold = 1
	}

	rows, err := pool.Query(ctx, `
SELECT org_login,
       COALESCE(SUM(occurrences), 0)::int      AS total,
       COUNT(DISTINCT assigned_login)::int      AS distinct_assignees,
       COUNT(DISTINCT issue_number)::int        AS distinct_issues,
       MAX(last_seen_at)                        AS last_seen_at
FROM hackathon_oob_assignments
WHERE hackathon_id = $1
GROUP BY org_login
ORDER BY total DESC, last_seen_at DESC
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.OOBOrgSummaries: %w", err)
	}
	defer rows.Close()

	out := []OOBOrgSummary{}
	for rows.Next() {
		var s OOBOrgSummary
		if err := rows.Scan(&s.OrgLogin, &s.Total, &s.DistinctAssignees, &s.DistinctIssues, &s.LastSeenAt); err != nil {
			return nil, fmt.Errorf("hackathon.OOBOrgSummaries: scan: %w", err)
		}
		s.Threshold = threshold
		s.Flagged = s.Total >= threshold
		out = append(out, s)
	}
	return out, rows.Err()
}

// OOBAssignmentsForOrg returns the individual events behind an org's count, so
// a reviewer sees which issue, which account and when - not just a number.
func OOBAssignmentsForOrg(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, orgLogin string) ([]OOBAssignment, error) {
	rows, err := pool.Query(ctx, `
SELECT o.id, o.hackathon_id, o.project_id, p.github_full_name, o.org_login, o.issue_number,
       o.assigned_login, o.maintainer_user_id, o.reverted, o.reverted_at,
       o.occurrences, o.first_seen_at, o.last_seen_at
FROM hackathon_oob_assignments o
JOIN projects p ON p.id = o.project_id
WHERE o.hackathon_id = $1 AND LOWER(o.org_login) = LOWER($2)
ORDER BY o.last_seen_at DESC
`, hackathonID, orgLogin)
	if err != nil {
		return nil, fmt.Errorf("hackathon.OOBAssignmentsForOrg: %w", err)
	}
	defer rows.Close()

	out := []OOBAssignment{}
	for rows.Next() {
		var a OOBAssignment
		if err := rows.Scan(&a.ID, &a.HackathonID, &a.ProjectID, &a.RepoFullName, &a.OrgLogin,
			&a.IssueNumber, &a.AssignedLogin, &a.MaintainerID, &a.Reverted, &a.RevertedAt,
			&a.Occurrences, &a.FirstSeenAt, &a.LastSeenAt); err != nil {
			return nil, fmt.Errorf("hackathon.OOBAssignmentsForOrg: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
