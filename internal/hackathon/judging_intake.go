package hackathon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// closingKeywordSQL matches a GitHub closing keyword paired with a specific
// issue number in a PR body.
//
// Copied deliberately from the identical predicate in
// internal/handlers/issue_applications.go's Mine(): GitHub only auto-closes
// an issue when its own keyword+number appears, so "Closes #12, #34" does
// not link #34. Matching any number mentioned anywhere would wrongly pull
// unrelated issues into judging.
const closingKeywordSQL = `pr.body ~* ('(^|[^0-9A-Za-z])(close[sd]?|fix(e[sd])?|resolve[sd]?)[[:space:]]*:?[[:space:]]*#' || hi.issue_number::text || '([^0-9]|$)')`

// candidatePR is a merged PR that names a GrainHack issue, before any of
// §2.4's conditions have been checked.
type candidatePR struct {
	PRNumber    int
	AuthorLogin string
	MergedAt    *time.Time
	IssueID     uuid.UUID
	IssueNumber int
	HackathonID uuid.UUID
	IssueAuthor string
	StartsAt    *time.Time
	EndsAt      *time.Time
	GraceHours  int
	AssignedTo  *string
	IssueIsDocs bool
}

// SyncVerdicts creates or refreshes hackathon_verdicts rows for a project's
// merged PRs that name a GrainHack issue.
//
// **A row is created even when the PR does not qualify.** AI-specs.md §2.4
// lists six conditions, and failing any of them means no reward - but "why
// did this PR get nothing" is a question that gets asked during appeals, and
// the absence of a row is not an answer. A well-meaning outsider who fixed a
// GrainHack issue without being assigned it especially deserves to be told
// that, rather than discovering silence.
//
// Rows are created in a pre-judged state: diff_stats and the §5.1 pre-filter
// result are computed here, on real data, so stages 3-5 later only fill in
// the model verdict rather than creating anything.
//
// Called from syncjobs' existing per-project PR sync (§2.2's "reuse the sync
// path, don't add a webhook parser"). Failures are the caller's to log and
// swallow; nothing here is allowed to fail a sync job.
func SyncVerdicts(
	ctx context.Context,
	pool db.DBPool,
	gh *github.Client,
	accessToken string,
	projectID uuid.UUID,
	fullName string,
) error {
	candidates, err := loadCandidatePRs(ctx, pool, projectID)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err := syncOneVerdict(ctx, pool, gh, accessToken, projectID, fullName, c); err != nil {
			// One bad PR must not stop the rest of the project's PRs.
			return fmt.Errorf("verdict for PR #%d: %w", c.PRNumber, err)
		}
	}
	return nil
}

func loadCandidatePRs(ctx context.Context, pool db.DBPool, projectID uuid.UUID) ([]candidatePR, error) {
	rows, err := pool.Query(ctx, `
SELECT pr.number, COALESCE(pr.author_login, ''), pr.merged_at_github,
       hi.id, hi.issue_number, hi.hackathon_id,
       COALESCE(gi.author_login, ''),
       h.starts_at, h.ends_at, COALESCE(h.merge_grace_period_hours, 48),
       a.github_login,
       COALESCE(hi.difficulty_tier, '') = 'docs'
FROM github_pull_requests pr
JOIN hackathon_issues hi ON hi.project_id = pr.project_id
JOIN hackathons h ON h.id = hi.hackathon_id
LEFT JOIN github_issues gi ON gi.project_id = hi.project_id AND gi.number = hi.issue_number
LEFT JOIN hackathon_assignments a
       ON a.hackathon_issue_id = hi.id AND a.status IN ('active','pr_submitted','completed')
WHERE pr.project_id = $1
  AND pr.merged
  -- The project must have an accepted application into this hackathon
  -- (§2.4 condition 4). Without it the issue was never in the event.
  AND EXISTS (
    SELECT 1 FROM hackathon_project_applications hpa
    WHERE hpa.hackathon_id = hi.hackathon_id AND hpa.project_id = pr.project_id
      AND hpa.status = 'accepted'
  )
  AND `+closingKeywordSQL+`
ORDER BY pr.number
`, projectID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.loadCandidatePRs: %w", err)
	}
	defer rows.Close()

	var out []candidatePR
	for rows.Next() {
		var c candidatePR
		if err := rows.Scan(&c.PRNumber, &c.AuthorLogin, &c.MergedAt, &c.IssueID, &c.IssueNumber,
			&c.HackathonID, &c.IssueAuthor, &c.StartsAt, &c.EndsAt, &c.GraceHours,
			&c.AssignedTo, &c.IssueIsDocs); err != nil {
			return nil, fmt.Errorf("hackathon.loadCandidatePRs: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// qualificationFailure applies AI-specs.md §2.4's conditions that can be
// answered without fetching the diff, returning the reason a PR does not
// qualify or "" if it does.
//
// Conditions 3 (linked to a GrainHack issue) and 4 (accepted project) are
// already guaranteed by loadCandidatePRs' WHERE clause - a PR that fails
// either was never a candidate.
func qualificationFailure(c candidatePR) string {
	// Condition 1: merged. Guaranteed by the query, but a merged PR with no
	// merge timestamp can't be checked against the window either.
	if c.MergedAt == nil {
		return "The PR is recorded as merged but has no merge time, so it can't be checked against the event window."
	}

	// Condition 2: merged inside the window plus the grace period. The grace
	// exists because a PR opened an hour before close cannot realistically
	// be reviewed and merged before it - without it, maintainer latency
	// costs the contributor.
	if c.StartsAt != nil && c.MergedAt.Before(*c.StartsAt) {
		return "The PR was merged before this GrainHack started."
	}
	if c.EndsAt != nil {
		deadline := c.EndsAt.Add(time.Duration(c.GraceHours) * time.Hour)
		if c.MergedAt.After(deadline) {
			return fmt.Sprintf("The PR was merged after the event ended, past the %d-hour grace period.", c.GraceHours)
		}
	}

	// Condition 5. The one most likely to surprise someone: an outsider can
	// fix a GrainHack issue in good faith and have it merged, and needs to
	// be told plainly why it earned nothing rather than left guessing.
	if c.AssignedTo == nil {
		return "Nobody was assigned this issue through Grainlify, so no PR against it qualifies. GrainHack issues are only claimable by applying on the platform."
	}
	if !sameLogin(*c.AssignedTo, c.AuthorLogin) {
		return fmt.Sprintf("The issue was assigned to @%s through Grainlify, not to the PR author. Only the assigned contributor's PR qualifies.", *c.AssignedTo)
	}

	// Condition 6's first half; the repo-admin/org-owner half needs GitHub
	// and is applied by the pre-filter with the data it has.
	if c.IssueAuthor != "" && sameLogin(c.IssueAuthor, c.AuthorLogin) {
		return "The PR author opened the issue, so it doesn't qualify for a reward."
	}
	return ""
}

// sameLogin compares GitHub logins, which are case-insensitive. Empty never
// matches empty: an unknown login must not silently equal another unknown.
func sameLogin(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func syncOneVerdict(
	ctx context.Context,
	pool db.DBPool,
	gh *github.Client,
	accessToken string,
	projectID uuid.UUID,
	fullName string,
	c candidatePR,
) error {
	// Never re-derive a verdict a human has already settled.
	var overriddenAt *time.Time
	err := pool.QueryRow(ctx, `
SELECT overridden_at FROM hackathon_verdicts
WHERE hackathon_id = $1 AND project_id = $2 AND pr_number = $3
`, c.HackathonID, projectID, c.PRNumber).Scan(&overriddenAt)
	if err == nil && overriddenAt != nil {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load existing verdict: %w", err)
	}

	// "pending" means stage 1 has not run yet, which is the honest state for
	// a PR whose diff we haven't read. Only a PR that actually survived the
	// pre-filter is marked "passed" - otherwise a row would look like it had
	// cleared a check that never happened.
	status := "pending"
	reason := qualificationFailure(c)
	if reason != "" {
		status = "rejected"
	}

	// Diff stats are only worth fetching for PRs still in contention -
	// they cost a GitHub call each, and a PR that already failed §2.4
	// isn't going to be judged.
	var statsJSON []byte
	if status == "pending" && gh != nil {
		files, truncated, ferr := gh.ListPRFiles(ctx, accessToken, fullName, c.PRNumber)
		if ferr != nil {
			// A diff we can't read is not a PR we can reject: the row stays
			// pending and the next sync retries it, rather than recording a
			// verdict derived from nothing.
			_ = ferr
		} else {
			changes := make([]FileChange, 0, len(files))
			for _, f := range files {
				changes = append(changes, FileChange{
					Filename: f.Filename, Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
				})
			}
			stats := ComputeDiffStats(changes)
			minLines, _ := EffectiveValue(ctx, pool, &c.HackathonID, "qualifying_pr_min_meaningful_lines")

			res := Prefilter(PrefilterInput{
				Stats: stats,
				// CI status isn't synced today, so it stays unknown - which
				// Prefilter deliberately treats as "not failing" rather than
				// inventing a rejection.
				CIPassed:                    nil,
				LinkedToGrainHackIssue:      true,
				AuthorIsAssignedContributor: true,
				IssueWasDocsIssue:           c.IssueIsDocs,
				MinMeaningfulLines:          atoiOr(minLines, 0),
			})
			if res.Rejected {
				status, reason = "rejected", res.Reason
			} else {
				status = "passed"
			}

			payload := map[string]any{
				"files_changed":      stats.FilesChanged,
				"lines_added":        stats.LinesAdded,
				"lines_removed":      stats.LinesRemoved,
				"generated_lines":    stats.GeneratedLines,
				"lockfile_lines":     stats.LockfileLines,
				"test_lines":         stats.TestLines,
				"tests_added":        stats.TestsAdded,
				"touches_core_paths": stats.TouchesCorePath,
				"meaningful_lines":   stats.MeaningfulLines,
				"docs_only":          stats.DocsOnly,
			}
			if truncated {
				// A verdict computed from a partial diff has to be visibly
				// partial, or a reviewer trusts numbers that undercount.
				payload["diff_truncated"] = true
			}
			statsJSON, _ = json.Marshal(payload)
		}
	}

	var userID *uuid.UUID
	var uid uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT user_id FROM github_accounts WHERE lower(login) = lower($1)`, c.AuthorLogin).Scan(&uid); err == nil {
		userID = &uid
	}

	_, err = pool.Exec(ctx, `
INSERT INTO hackathon_verdicts
  (hackathon_id, hackathon_issue_id, project_id, pr_number, user_id, github_login,
   prefilter_status, prefilter_reason, diff_stats)
VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)
ON CONFLICT (hackathon_id, project_id, pr_number) DO UPDATE
SET prefilter_status = EXCLUDED.prefilter_status,
    prefilter_reason = EXCLUDED.prefilter_reason,
    hackathon_issue_id = EXCLUDED.hackathon_issue_id,
    user_id = COALESCE(EXCLUDED.user_id, hackathon_verdicts.user_id),
    -- Keep the stats we already have if this pass couldn't fetch them.
    diff_stats = COALESCE(EXCLUDED.diff_stats, hackathon_verdicts.diff_stats),
    updated_at = now()
WHERE hackathon_verdicts.overridden_at IS NULL
`, c.HackathonID, c.IssueID, projectID, c.PRNumber, userID, c.AuthorLogin, status, reason, statsJSON)
	if err != nil {
		return fmt.Errorf("upsert verdict: %w", err)
	}
	return nil
}
