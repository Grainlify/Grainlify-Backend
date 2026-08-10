package hackathon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/email"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// assignmentTickInterval is how often the assignment pipeline sweeps for
// windows that have closed, assignments that have gone stale, and events
// about to end.
//
// One minute rather than the reconciler's five: application_window_hours can
// legitimately be set to 1 for a short event, and a draw running up to five
// minutes after its window closed is a visible delay to whoever is waiting.
const assignmentTickInterval = time.Minute

// endOfEventWarningLead is how far ahead of ends_at contributors holding an
// open assignment are warned (§13 #2: notified *before* ends_at, not after).
const endOfEventWarningLead = 24 * time.Hour

// AssignmentRunner drives every time-based step of AI-specs.md §4: closing
// application windows and running their draws, releasing stale assignments,
// and warning contributors before the event ends.
//
// Deliberately separate from Reconciler: that one only re-enqueues sync
// jobs and is safe to run anywhere, whereas this one writes assignments and
// calls GitHub.
type AssignmentRunner struct {
	pool     db.DBPool
	gh       *github.Client
	notifier *notifications.Service
	// installationToken resolves a GitHub App installation token for a
	// project, so the runner can set the GitHub assignee after a draw.
	// May be nil, in which case assignments are recorded in Grainlify but
	// not mirrored to GitHub.
	installationToken func(ctx context.Context, projectID uuid.UUID) (string, error)
}

func NewAssignmentRunner(
	pool db.DBPool,
	gh *github.Client,
	notifier *notifications.Service,
	installationToken func(ctx context.Context, projectID uuid.UUID) (string, error),
) *AssignmentRunner {
	return &AssignmentRunner{pool: pool, gh: gh, notifier: notifier, installationToken: installationToken}
}

// NewAssignmentRunnerFromConfig assembles a runner with the same dependency
// wiring syncjobs.New already uses, so cmd/api doesn't have to hand-build a
// mailer and a token resolver. Mirrors that constructor's shape deliberately.
func NewAssignmentRunnerFromConfig(cfg config.Config, pool db.DBPool) *AssignmentRunner {
	var mailer email.Mailer
	if m := email.NewMailerCloudMailer(cfg.MailerCloudAPIKey, cfg.EmailFromAddress, cfg.EmailFromName); m != nil {
		mailer = m
	}
	notify := notifications.New(&db.DB{Pool: pool}, mailer, cfg.FrontendBaseURL)

	tokenFn := func(ctx context.Context, projectID uuid.UUID) (string, error) {
		var installationID string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(github_app_installation_id, '') FROM projects WHERE id = $1`, projectID).Scan(&installationID); err != nil {
			return "", err
		}
		if installationID == "" {
			return "", fmt.Errorf("project %s has no github app installation", projectID)
		}
		appClient, err := github.NewGitHubAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
		if err != nil {
			return "", err
		}
		return appClient.GetInstallationToken(ctx, installationID)
	}

	return NewAssignmentRunner(pool, github.NewClient(), notify, tokenFn)
}

// Run blocks, ticking until ctx is cancelled. Mirrors Reconciler.Run's shape.
func (r *AssignmentRunner) Run(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	ticker := time.NewTicker(assignmentTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Each step is independent: one failing must not stop the
			// others, since they cover different failure modes (a stuck
			// draw shouldn't also stop stale release).
			if err := r.runDueDraws(ctx); err != nil {
				slog.Warn("hackathon: due draws failed", "error", err)
			}
			if err := r.releaseStale(ctx); err != nil {
				slog.Warn("hackathon: stale release failed", "error", err)
			}
			if err := r.warnEndOfEvent(ctx); err != nil {
				slog.Warn("hackathon: end-of-event warning failed", "error", err)
			}
			if err := r.sweepClosedEvents(ctx); err != nil {
				slog.Warn("hackathon: closed-event sweep failed", "error", err)
			}
		}
	}
}

// dueIssue is one published issue whose application window has closed with
// no active assignment.
type dueIssue struct {
	hackathonID uuid.UUID
	issueID     uuid.UUID
	projectID   uuid.UUID
	issueNumber int
	applicants  int
	drawsSoFar  int
}

// runDueDraws finds every issue whose window has closed and resolves it -
// either by running the draw, or (when nobody applied) by reopening the
// window up to empty_window_retries times.
func (r *AssignmentRunner) runDueDraws(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
SELECT hi.hackathon_id, hi.id, hi.project_id, hi.issue_number,
  (SELECT count(*) FROM hackathon_issue_applications a
     WHERE a.hackathon_issue_id = hi.id AND a.status = 'applied'),
  (SELECT count(*) FROM hackathon_draws d
     WHERE d.hackathon_issue_id = hi.id AND NOT d.is_simulation)
FROM hackathon_issues hi
JOIN hackathons h ON h.id = hi.hackathon_id
WHERE h.phase = 'live'
  AND hi.status = 'published'
  AND hi.application_window_closes_at IS NOT NULL
  AND hi.application_window_closes_at <= now()
  AND NOT EXISTS (
    SELECT 1 FROM hackathon_assignments x
    WHERE x.hackathon_issue_id = hi.id AND x.status IN ('active', 'pr_submitted')
  )
-- §3.7 draw_order is 'randomised': issues are processed one at a time in
-- random order so that, under sequential slot consumption, which issue a
-- multi-applicant contributor wins isn't decided by issue number.
ORDER BY random()
`)
	if err != nil {
		return fmt.Errorf("hackathon.runDueDraws: query: %w", err)
	}
	var due []dueIssue
	for rows.Next() {
		var d dueIssue
		if err := rows.Scan(&d.hackathonID, &d.issueID, &d.projectID, &d.issueNumber, &d.applicants, &d.drawsSoFar); err != nil {
			rows.Close()
			return fmt.Errorf("hackathon.runDueDraws: scan: %w", err)
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, d := range due {
		// Nobody applied: reopen rather than draw, until retries run out.
		// §3.7's empty_window_retries exists because an unassigned issue
		// helps nobody, and a quiet first day is normal.
		if d.applicants == 0 {
			retries, err := EffectiveValue(ctx, r.pool, &d.hackathonID, "empty_window_retries")
			if err != nil {
				slog.Warn("hackathon: read empty_window_retries", "error", err)
				continue
			}
			if d.drawsSoFar < atoiOr(retries, 3) {
				if err := ReopenWindow(ctx, r.pool, d.hackathonID, d.issueID); err != nil {
					slog.Warn("hackathon: reopen window", "issue_id", d.issueID, "error", err)
				}
				// Record the empty round so drawsSoFar advances and the
				// retries actually terminate.
				if _, err := RunDraw(ctx, r.pool, d.hackathonID, d.issueID, 0, false); err != nil {
					slog.Warn("hackathon: record empty draw", "issue_id", d.issueID, "error", err)
				}
				continue
			}
		}

		res, err := RunDraw(ctx, r.pool, d.hackathonID, d.issueID, 0, false)
		if err != nil {
			slog.Warn("hackathon: draw failed", "issue_id", d.issueID, "error", err)
			continue
		}
		if res.WinnerUserID == nil {
			continue
		}
		r.onAssigned(ctx, d, res)
	}
	return nil
}

// onAssigned mirrors a fresh assignment out to GitHub and tells the winner.
// Best-effort: the Grainlify assignment is already committed and is the
// source of truth (§2.3), so a GitHub or email failure must not undo it.
func (r *AssignmentRunner) onAssigned(ctx context.Context, d dueIssue, res *DrawResult) {
	var fullName string
	if err := r.pool.QueryRow(ctx, `SELECT github_full_name FROM projects WHERE id = $1`, d.projectID).Scan(&fullName); err != nil {
		slog.Warn("hackathon: load project for assignment", "error", err)
		return
	}

	if r.gh != nil && r.installationToken != nil {
		if tok, err := r.installationToken(ctx, d.projectID); err == nil {
			if err := r.gh.AddIssueAssignees(ctx, tok, fullName, d.issueNumber, []string{res.WinnerLogin}); err != nil {
				slog.Warn("hackathon: set github assignee", "repo", fullName, "issue", d.issueNumber, "error", err)
			}
		}
	}

	if r.notifier != nil && res.WinnerUserID != nil {
		r.notifier.Notify(ctx, *res.WinnerUserID, notifications.TypeGrainHackAssigned,
			"You've been assigned a GrainHack issue",
			fmt.Sprintf("You won the draw for %s#%d. Submit a PR before the deadline to keep the assignment.", fullName, d.issueNumber),
			"",
		)
	}
}

func (r *AssignmentRunner) releaseStale(ctx context.Context) error {
	released, err := ReleaseStale(ctx, r.pool)
	if err != nil {
		return err
	}
	for _, rel := range released {
		// Take the GitHub assignee back off so the issue visibly returns to
		// the pool rather than looking claimed by someone who has stopped.
		r.unassignOnGitHub(ctx, rel)
		if r.notifier != nil {
			r.notifier.Notify(ctx, rel.UserID, notifications.TypeGrainHackAssignmentReleased,
				"Your GrainHack assignment was released",
				"You didn't submit a qualifying PR before the deadline, so the issue has gone back into the pool and this counts as an abandon.",
				"",
			)
		}
		// Put the issue straight back up for a new draw.
		if err := ReopenWindow(ctx, r.pool, rel.HackathonID, rel.IssueID); err != nil {
			slog.Warn("hackathon: reopen window after stale release", "issue_id", rel.IssueID, "error", err)
		}
	}
	return nil
}

// sweepClosedEvents re-runs the close cleanup for any hackathon already in
// 'closed' that still has open assignments or unreverted issues.
//
// Transition does this inline, but deliberately outside its transaction -
// the admin's phase change must land even if releasing a hundred
// assignments hits a problem. This is the convergence path for that case,
// and it's also where the GitHub unassign happens, since Transition has no
// GitHub client of its own.
func (r *AssignmentRunner) sweepClosedEvents(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
SELECT DISTINCT h.id
FROM hackathons h
WHERE h.phase = 'closed'
  AND (
    EXISTS (SELECT 1 FROM hackathon_assignments a
            WHERE a.hackathon_id = h.id AND a.status IN ('active','pr_submitted'))
    OR EXISTS (SELECT 1 FROM hackathon_issues hi
               WHERE hi.hackathon_id = h.id AND hi.status = 'published'
                 AND NOT EXISTS (SELECT 1 FROM hackathon_assignments a2
                                 WHERE a2.hackathon_issue_id = hi.id AND a2.status = 'completed'))
  )
`)
	if err != nil {
		return fmt.Errorf("hackathon.sweepClosedEvents: %w", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		released, err := CloseEventAssignments(ctx, r.pool, id)
		if err != nil {
			slog.Warn("hackathon: close cleanup", "hackathon_id", id, "error", err)
			continue
		}
		for _, rel := range released {
			r.unassignOnGitHub(ctx, rel)
			if r.notifier != nil {
				r.notifier.Notify(ctx, rel.UserID, notifications.TypeGrainHackEventEnding,
					"The GrainHack ended before your issue was merged",
					"The event has closed, so this assignment was released and the issue has gone back to being a normal Grainlify bounty. A PR merged inside the grace period still counts.",
					"",
				)
			}
		}
	}
	return nil
}

// unassignOnGitHub clears the GitHub assignee for a released assignment, so
// the issue doesn't keep looking claimed. Best-effort by design: the
// Grainlify record is the source of truth (§2.3).
func (r *AssignmentRunner) unassignOnGitHub(ctx context.Context, rel ReleasedAssignment) {
	if r.gh == nil || r.installationToken == nil {
		return
	}
	var fullName string
	if err := r.pool.QueryRow(ctx, `SELECT github_full_name FROM projects WHERE id = $1`, rel.ProjectID).Scan(&fullName); err != nil {
		return
	}
	tok, err := r.installationToken(ctx, rel.ProjectID)
	if err != nil {
		return
	}
	_ = r.gh.RemoveIssueAssignees(ctx, tok, fullName, rel.IssueNumber, []string{rel.GitHubLogin})
}

func (r *AssignmentRunner) warnEndOfEvent(ctx context.Context) error {
	pending, err := PendingEndOfEventWarning(ctx, r.pool, endOfEventWarningLead)
	if err != nil {
		return err
	}
	if r.notifier == nil {
		return nil
	}
	for _, p := range pending {
		r.notifier.Notify(ctx, p.UserID, notifications.TypeGrainHackEventEnding,
			"Your GrainHack issue closes soon",
			"This event ends within 24 hours. A PR merged inside the grace period still counts; otherwise the assignment is released and the issue reverts to a normal Grainlify bounty.",
			"",
		)
	}
	return nil
}
