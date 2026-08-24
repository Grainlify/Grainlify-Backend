package hackathon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ErrNoActiveAssignment is returned when a lifecycle action targets an
// assignment that isn't open - already released, already completed, or
// belonging to someone else.
var ErrNoActiveAssignment = errors.New("no active assignment")

// slotFreedOnSubmission reports whether config frees the slot at PR
// submission (the default) rather than at merge (§3.4's slot_freed_on).
func slotFreedOnSubmission(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (bool, error) {
	v, err := EffectiveValue(ctx, pool, &hackathonID, "slot_freed_on")
	if err != nil {
		return false, err
	}
	return v != "pr_merge", nil
}

// RecordQualifyingPR applies AI-specs.md §4.6's "Qualifying PR submitted →
// slot freed immediately, timer stops".
//
// Whether a PR *qualifies* is §3.5's separate question (non-draft, CI green,
// links the issue, enough meaningful lines); the caller decides that and
// calls this only for PRs that pass. Keeping the two apart means the
// qualification rules can tighten without touching lifecycle bookkeeping.
func RecordQualifyingPR(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber, prNumber int, login string) error {
	freeNow, err := slotFreedOnSubmission(ctx, pool, hackathonID)
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `
UPDATE hackathon_assignments
SET status = 'pr_submitted',
    qualifying_pr_number = $4,
    qualifying_pr_at = COALESCE(qualifying_pr_at, now()),
    -- Stops the stale timer either way: the contributor has demonstrably
    -- done the work, so they must not be auto-released while waiting on a
    -- maintainer to review.
    stale_at = NULL,
    holds_slot = CASE WHEN $5 THEN false ELSE holds_slot END,
    updated_at = now()
WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = $3
  AND lower(github_login) = lower($6)
  AND status = 'active'
`, hackathonID, projectID, issueNumber, prNumber, freeNow, login)
	if err != nil {
		return fmt.Errorf("hackathon.RecordQualifyingPR: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoActiveAssignment
	}
	return nil
}

// RecordMerge applies §4.6's "PR merged → enters judging pool". Also frees
// the slot when slot_freed_on is pr_merge.
func RecordMerge(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber int, login string) error {
	tag, err := pool.Exec(ctx, `
UPDATE hackathon_assignments
SET status = 'completed',
    merged_at = COALESCE(merged_at, now()),
    holds_slot = false,
    stale_at = NULL,
    updated_at = now()
WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = $3
  AND lower(github_login) = lower($4)
  AND status IN ('active', 'pr_submitted')
`, hackathonID, projectID, issueNumber, login)
	if err != nil {
		return fmt.Errorf("hackathon.RecordMerge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoActiveAssignment
	}
	return nil
}

// ReleaseVoluntary applies §4.6's two voluntary-release rows: inside
// voluntary_release_grace_hours the slot frees with no abandon; after it,
// the slot frees and an abandon is recorded.
//
// The grace window exists so that a contributor who realises within a day
// that an issue isn't for them hands it back instead of sitting on it until
// the stale timer fires - which would cost the issue several days and them
// an abandon.
func ReleaseVoluntary(ctx context.Context, pool db.DBPool, hackathonID, userID uuid.UUID, assignmentID uuid.UUID) (abandoned bool, err error) {
	graceStr, err := EffectiveValue(ctx, pool, &hackathonID, "voluntary_release_grace_hours")
	if err != nil {
		return false, err
	}
	grace := time.Duration(atoiOr(graceStr, 48)) * time.Hour

	var assignedAt time.Time
	err = pool.QueryRow(ctx, `
SELECT assigned_at FROM hackathon_assignments
WHERE id = $1 AND user_id = $2 AND status IN ('active', 'pr_submitted')
`, assignmentID, userID).Scan(&assignedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNoActiveAssignment
	}
	if err != nil {
		return false, fmt.Errorf("hackathon.ReleaseVoluntary: load: %w", err)
	}

	abandoned = time.Since(assignedAt) > grace
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_assignments
SET status = 'released_voluntary',
    holds_slot = false,
    stale_at = NULL,
    released_at = now(),
    release_reason = $2,
    abandon_recorded = $3,
    updated_at = now()
WHERE id = $1
`, assignmentID,
		map[bool]string{true: "voluntary release after grace period", false: "voluntary release within grace period"}[abandoned],
		abandoned); err != nil {
		return false, fmt.Errorf("hackathon.ReleaseVoluntary: update: %w", err)
	}
	return abandoned, nil
}

// ReleasedAssignment describes one auto-released assignment, so the caller
// can notify the contributor and re-open the issue for a fresh draw.
type ReleasedAssignment struct {
	AssignmentID uuid.UUID
	HackathonID  uuid.UUID
	IssueID      uuid.UUID
	ProjectID    uuid.UUID
	IssueNumber  int
	UserID       uuid.UUID
	GitHubLogin  string
}

// ReleaseStale applies §4.6's "Stale timeout → auto-released, slot freed,
// abandon recorded" across every live hackathon.
//
// stale_at is stamped at assignment time from the config in force then, so
// an admin lengthening stale_assignment_days mid-event never retroactively
// rescues an assignment that already expired, and shortening it never
// retroactively kills one that was fine when it was made.
func ReleaseStale(ctx context.Context, pool db.DBPool) ([]ReleasedAssignment, error) {
	rows, err := pool.Query(ctx, `
UPDATE hackathon_assignments a
SET status = 'released_stale',
    holds_slot = false,
    released_at = now(),
    release_reason = 'no qualifying PR before the stale deadline',
    abandon_recorded = true,
    stale_at = NULL,
    updated_at = now()
FROM hackathons h
WHERE h.id = a.hackathon_id
  AND h.phase = 'live'
  AND a.status = 'active'
  AND a.stale_at IS NOT NULL
  AND a.stale_at < now()
RETURNING a.id, a.hackathon_id, a.hackathon_issue_id, a.project_id, a.issue_number, a.user_id, a.github_login
`)
	if err != nil {
		return nil, fmt.Errorf("hackathon.ReleaseStale: %w", err)
	}
	defer rows.Close()
	return scanReleased(rows)
}

// CloseEventAssignments implements the second half of AI-specs.md §13's
// question 2: a PR merged inside the grace period counts, otherwise the
// assignment is released and the issue reverts to a normal Grainlify bounty.
//
// Called on the live → closed transition. Assignments already in
// 'pr_submitted' are released too: submitting is not merging, and the merge
// window (ends_at + merge_grace_period_hours) is what decides whether that
// PR still counts - handled by the judging slice, which reads merged_at.
// Returns the released assignments so the caller can clear their GitHub
// assignees and notify the contributors.
func CloseEventAssignments(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) ([]ReleasedAssignment, error) {
	rows, err := pool.Query(ctx, `
UPDATE hackathon_assignments
SET status = 'released_event_end',
    holds_slot = false,
    stale_at = NULL,
    released_at = now(),
    release_reason = 'hackathon closed with the assignment still open',
    updated_at = now()
WHERE hackathon_id = $1 AND status IN ('active', 'pr_submitted')
RETURNING id, hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.CloseEventAssignments: %w", err)
	}
	released, err := scanReleased(rows)
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("hackathon.CloseEventAssignments: scan released: %w", err)
	}

	// Applications that never got drawn are moot once the event closes.
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issue_applications
SET status = 'lost', updated_at = now()
WHERE hackathon_id = $1 AND status = 'applied'
`, hackathonID); err != nil {
		return nil, fmt.Errorf("hackathon.CloseEventAssignments: resolve open applications: %w", err)
	}

	// The second half of §13 #2: the issue *reverts to a normal Grainlify
	// bounty*, it isn't merely unassigned. Releasing alone would leave it
	// sitting in 'published' inside a finished event - visible as a
	// GrainHack issue, with an application window nothing will ever draw,
	// and invisible to the ordinary issue_applications flow that should now
	// own it.
	//
	// Issues that were actually completed stay published: the judging slice
	// reads them, and rewriting their status would erase what the event
	// produced.
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issues hi
SET status = 'removed',
    removed_at = now(),
    application_window_opens_at = NULL,
    application_window_closes_at = NULL,
    updated_at = now()
WHERE hi.hackathon_id = $1
  AND hi.status = 'published'
  AND NOT EXISTS (
    SELECT 1 FROM hackathon_assignments a
    WHERE a.hackathon_issue_id = hi.id AND a.status = 'completed'
  )
`, hackathonID); err != nil {
		return nil, fmt.Errorf("hackathon.CloseEventAssignments: revert unfinished issues: %w", err)
	}
	return released, nil
}

// PendingEndOfEventWarning lists open assignments in hackathons ending
// within `within`, that haven't been warned yet. §13 #2 is explicit that the
// contributor is told *before* ends_at, not after - being notified after the
// fact that your in-flight work no longer counts is the worst version of
// this rule.
func PendingEndOfEventWarning(ctx context.Context, pool db.DBPool, within time.Duration) ([]ReleasedAssignment, error) {
	rows, err := pool.Query(ctx, `
UPDATE hackathon_assignments a
SET end_of_event_notified_at = now(), updated_at = now()
FROM hackathons h
WHERE h.id = a.hackathon_id
  AND h.phase = 'live'
  AND a.status IN ('active', 'pr_submitted')
  AND a.end_of_event_notified_at IS NULL
  AND h.ends_at IS NOT NULL
  AND h.ends_at > now()
  AND h.ends_at <= now() + $1::interval
RETURNING a.id, a.hackathon_id, a.hackathon_issue_id, a.project_id, a.issue_number, a.user_id, a.github_login
`, fmt.Sprintf("%d seconds", int(within.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("hackathon.PendingEndOfEventWarning: %w", err)
	}
	defer rows.Close()
	return scanReleased(rows)
}

func scanReleased(rows pgx.Rows) ([]ReleasedAssignment, error) {
	var out []ReleasedAssignment
	for rows.Next() {
		var r ReleasedAssignment
		if err := rows.Scan(&r.AssignmentID, &r.HackathonID, &r.IssueID, &r.ProjectID,
			&r.IssueNumber, &r.UserID, &r.GitHubLogin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveAssignee returns the GitHub login currently assigned to this issue
// through Grainlify, or "" if nobody is. Backs both isAssigned (intake.go)
// and §2.3's out-of-band assignment detection, which needs to compare the
// GitHub assignee against Grainlify's own record.
func ActiveAssignee(ctx context.Context, pool db.DBPool, projectID uuid.UUID, issueNumber int) (string, error) {
	var login string
	err := pool.QueryRow(ctx, `
SELECT github_login FROM hackathon_assignments
-- Who holds this issue, which includes whoever completed it: this reconciles
-- the GitHub assignee, and a merged issue must not read as unassigned.
WHERE project_id = $1 AND issue_number = $2 AND NOT hackathon_assignment_released(status)
ORDER BY assigned_at DESC
LIMIT 1
`, projectID, issueNumber).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("hackathon.ActiveAssignee: %w", err)
	}
	return login, nil
}
