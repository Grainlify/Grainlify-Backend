package syncjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/email"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type Worker struct {
	cfg      config.Config
	pool     db.DBPool
	limiter  *rate.Limiter
	gh       *github.Client
	notify   *notifications.Service
	workerID string
}

func New(cfg config.Config, pool db.DBPool) *Worker {
	// A separate, independently-constructed Service (rather than one shared
	// with internal/api.New, which has no way to hand this one back to
	// cmd/api/main.go today) - same construction as api.go's own notifSvc,
	// just built from the pieces this package already has (cfg, pool). Both
	// end up reading/writing the same notifications/notification_preferences
	// tables either way, so this duplication is harmless.
	var mailer email.Mailer
	if m := email.NewMailerCloudMailer(cfg.MailerCloudAPIKey, cfg.EmailFromAddress, cfg.EmailFromName); m != nil {
		mailer = m
	}
	notify := notifications.New(&db.DB{Pool: pool}, mailer, cfg.FrontendBaseURL)

	return &Worker{
		cfg:      cfg,
		pool:     pool,
		limiter:  rate.NewLimiter(rate.Every(250*time.Millisecond), 2), // ~4 req/s, burst 2
		gh:       github.NewClient(),
		notify:   notify,
		workerID: fmt.Sprintf("%s:%d", hostname(), os.Getpid()),
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.pool == nil {
		return fmt.Errorf("db not configured")
	}
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.processOne(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				slog.Error("sync worker error", "error", err)
			}
		}
	}
}

func (w *Worker) processOne(ctx context.Context) error {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var jobID uuid.UUID
	var projectID uuid.UUID
	var jobType string
	err = tx.QueryRow(ctx, `
SELECT id, project_id, job_type
FROM sync_jobs
WHERE status = 'pending'
  AND run_at <= now()
ORDER BY run_at ASC
FOR UPDATE SKIP LOCKED
LIMIT 1
`).Scan(&jobID, &projectID, &jobType)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
UPDATE sync_jobs
SET status = 'running', locked_at = now(), locked_by = $2, updated_at = now()
WHERE id = $1
`, jobID, w.workerID)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	runErr := w.runJob(ctx, jobID, projectID, jobType)

	status := "completed"
	lastErr := ""
	if runErr != nil {
		status = "failed"
		lastErr = runErr.Error()
	}

	_, _ = w.pool.Exec(ctx, `
UPDATE sync_jobs
SET status = $2, attempts = attempts + 1, last_error = NULLIF($3, ''), updated_at = now()
WHERE id = $1
`, jobID, status, lastErr)

	return nil
}

func (w *Worker) runJob(ctx context.Context, jobID uuid.UUID, projectID uuid.UUID, jobType string) error {
	// Load project + owner to get GitHub token.
	var fullName string
	var ownerUserID uuid.UUID
	var installationID string
	err := w.pool.QueryRow(ctx, `
SELECT github_full_name, owner_user_id, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1
`, projectID).Scan(&fullName, &ownerUserID, &installationID)
	if err != nil {
		slog.Error("sync job failed: project not found",
			"job_id", jobID,
			"project_id", projectID,
			"error", err,
		)
		return err
	}

	linked, err := github.GetLinkedAccount(ctx, w.pool, ownerUserID, w.cfg.TokenEncKeyB64)
	if err != nil {
		slog.Error("sync job failed: GitHub account not linked",
			"job_id", jobID,
			"project_id", projectID,
			"user_id", ownerUserID,
			"repo", fullName,
			"error", err,
			"hint", "User needs to link their GitHub account via OAuth",
		)
		return fmt.Errorf("github_not_linked: %w", err)
	}

	slog.Info("starting sync job",
		"job_id", jobID,
		"job_type", jobType,
		"project_id", projectID,
		"repo", fullName,
		"user_id", ownerUserID,
	)

	var syncErr error
	switch jobType {
	case "sync_issues":
		syncErr = w.syncIssues(ctx, projectID, fullName, linked.AccessToken, installationID)
	case "sync_prs":
		syncErr = w.syncPRs(ctx, projectID, fullName, linked.AccessToken)
	default:
		syncErr = fmt.Errorf("unknown job_type: %s", jobType)
	}

	if syncErr != nil {
		slog.Error("sync job failed",
			"job_id", jobID,
			"job_type", jobType,
			"project_id", projectID,
			"repo", fullName,
			"error", syncErr,
		)
		return syncErr
	}

	slog.Info("sync job completed successfully",
		"job_id", jobID,
		"job_type", jobType,
		"project_id", projectID,
		"repo", fullName,
	)
	return nil
}

func (w *Worker) syncIssues(ctx context.Context, projectID uuid.UUID, fullName string, token string, installationID string) error {
	totalIssues := 0

	// Installation token for reconciliation writes (removing an ineligible
	// assignee, posting the explanatory comment) - fetched at most once for
	// this whole call, and only if an issue actually needs it.
	var instToken string
	var instTokenFetched bool
	var instTokenErr error
	getInstallationToken := func() (string, error) {
		if instTokenFetched {
			return instToken, instTokenErr
		}
		instTokenFetched = true
		if installationID == "" {
			instTokenErr = fmt.Errorf("project has no github app installation")
			return "", instTokenErr
		}
		appClient, err := github.NewGitHubAppClient(w.cfg.GitHubAppID, w.cfg.GitHubAppPrivateKey)
		if err != nil {
			instTokenErr = err
			return "", err
		}
		instToken, instTokenErr = appClient.GetInstallationToken(ctx, installationID)
		return instToken, instTokenErr
	}

	// Widens the legacy literal below to also cover this project's active
	// hackathon's configured grainhack_label, if any (internal/hackathon's
	// EffectiveGrainHackLabels) - computed once per call, not per issue,
	// since it only depends on projectID. A resolution failure falls back
	// to nil, which shouldEnforceAssignmentEligibility itself treats as
	// "just the legacy literal", so this never regresses existing behavior.
	candidateLabels, err := hackathon.EffectiveGrainHackLabels(ctx, w.pool, projectID)
	if err != nil {
		slog.Warn("failed to resolve effective GrainHack labels, falling back to legacy literal",
			"project_id", projectID, "error", err)
		candidateLabels = nil
	}

	// Primary language for a freshly-intake hackathon_issues row - fetched
	// at most once for this whole call, same lazy-once shape as
	// getInstallationToken above, and only if an issue actually needs it.
	var primaryLang string
	var primaryLangFetched bool
	getPrimaryLanguage := func() (string, error) {
		if primaryLangFetched {
			return primaryLang, nil
		}
		primaryLangFetched = true
		langs, err := w.gh.GetRepoLanguages(ctx, token, fullName)
		if err != nil {
			return "", err
		}
		var maxBytes int64
		for lang, bytes := range langs {
			if bytes > maxBytes {
				maxBytes = bytes
				primaryLang = lang
			}
		}
		return primaryLang, nil
	}

	for page := 1; page <= 50; page++ { // safety cap
		if err := w.limiter.Wait(ctx); err != nil {
			return err
		}
		items, err := w.gh.ListIssuesPage(ctx, token, fullName, page)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}

		for _, it := range items {
			// Skip PRs from the issues endpoint.
			if it.PullRequest != nil {
				continue
			}
			totalIssues++

			ghLogins := make([]string, len(it.Assignees))
			for i, a := range it.Assignees {
				ghLogins[i] = a.Login
			}

			// Grainlify is the source of truth for who may be assigned: an
			// assignee with no eligible issue_applications row (e.g. assigned
			// directly on GitHub, bypassing the platform) gets removed here,
			// and issue_applications.status is reconciled in both directions
			// regardless of which interface made the change. Scoped to open
			// issues carrying the GrainHack label only - closed issues (the
			// work is done, nothing to enforce) and issues outside the
			// hackathon are never touched by this, regardless of assignee.
			var ineligible []string
			if shouldEnforceAssignmentEligibility(it.State, it.Labels, candidateLabels) {
				var recErr error
				ineligible, recErr = reconcileApplicationStatuses(ctx, w.pool, projectID, it.Number, ghLogins)
				if recErr != nil {
					slog.Warn("reconcile application statuses failed",
						"project_id", projectID, "issue_number", it.Number, "error", recErr)
					ineligible = nil
				}
			}

			// GrainHack Phase-2 issue intake (AI-specs.md §2.2) - keeps this
			// project's hackathon_issues row in sync with the issue's
			// current label/state, if it belongs to an active hackathon at
			// all. Best-effort: never fails the surrounding sync job.
			labelNames := make([]string, len(it.Labels))
			for i, l := range it.Labels {
				labelNames[i] = l.Name
			}
			if err := hackathon.SyncIssueLabel(ctx, w.pool, w.gh, w.notify, getInstallationToken,
				projectID, fullName, it.Number, labelNames, strings.EqualFold(it.State, "open"), getPrimaryLanguage,
			); err != nil {
				slog.Warn("hackathon issue intake failed",
					"project_id", projectID, "issue_number", it.Number, "error", err)
			}

			removed := make(map[string]bool, len(ineligible))
			commentPosted := false
			// auto_revert_oob_assignment gates the GitHub write only. The
			// issue_applications reconciliation above still runs either way:
			// an admin switching this off is asking us to stop touching their
			// repository, not asking us to stop keeping accurate records.
			//
			// This gate did not exist until now - the setting was seeded and
			// shown in the admin UI while being read by nothing, so an admin
			// could turn it off, see it off, and Grainlify would keep removing
			// assignees and commenting on repositories it does not own.
			//
			// Counting these against oob_assignment_flag_threshold (§2.3) is
			// still to come; until it lands, switching this off means the
			// out-of-band assignment leaves no record beyond this log line.
			mayRevert := len(ineligible) > 0 && hackathon.AutoRevertOOBAssignment(ctx, w.pool, projectID)
			if len(ineligible) > 0 && !mayRevert {
				slog.Info("out-of-band assignees left in place: auto_revert_oob_assignment is off",
					"project_id", projectID, "issue_number", it.Number, "logins", ineligible)
			} else if len(ineligible) > 0 {
				if tok, tokErr := getInstallationToken(); tokErr != nil {
					slog.Warn("no installation token available to remove ineligible assignees",
						"project_id", projectID, "issue_number", it.Number, "logins", ineligible, "error", tokErr)
				} else if err := w.limiter.Wait(ctx); err != nil {
					return err
				} else if err := w.gh.RemoveIssueAssignees(ctx, tok, fullName, it.Number, ineligible); err != nil {
					slog.Warn("failed to remove ineligible assignees",
						"project_id", projectID, "issue_number", it.Number, "logins", ineligible, "error", err)
				} else {
					for _, login := range ineligible {
						removed[strings.ToLower(login)] = true
						if err := w.limiter.Wait(ctx); err != nil {
							continue
						}
						body := fmt.Sprintf("@%s was removed as assignee: no matching application for this issue was found on Grainlify. Maintainers can only assign contributors who applied through the platform.", login)
						if _, err := w.gh.CreateIssueComment(ctx, tok, fullName, it.Number, body); err != nil {
							slog.Warn("failed to post reconciliation bot comment",
								"project_id", projectID, "issue_number", it.Number, "login", login, "error", err)
							continue
						}
						commentPosted = true
					}
				}
			}

			// §2.3 step 3: record the event against the maintainer and org.
			// Runs on both paths - whether or not the revert fired, and
			// whether or not it succeeded. auto_revert_oob_assignment decides
			// whether Grainlify writes to someone else's repository; it does
			// not decide whether Grainlify notices. Since this count feeds
			// maintainer-pool eligibility (§7), a switch that also erased the
			// evidence would be the first thing a maintainer gaming the event
			// would turn off.
			//
			// `removed` is consulted rather than `mayRevert` so the stored
			// flag reflects what actually happened on GitHub: an attempted
			// revert that failed is recorded as not reverted.
			for _, login := range ineligible {
				if err := hackathon.RecordOOBAssignment(ctx, w.pool, projectID,
					it.Number, login, removed[strings.ToLower(login)],
				); err != nil {
					slog.Warn("failed to record out-of-band assignment",
						"project_id", projectID, "issue_number", it.Number, "login", login, "error", err)
				}
			}

			// Filter out only what was actually removed on GitHub - if
			// removal failed above, assigneesJSON should still reflect
			// reality (they're still assigned) rather than what we merely
			// attempted.
			finalAssignees := make([]struct {
				Login string `json:"login"`
			}, 0, len(it.Assignees))
			for _, a := range it.Assignees {
				if !removed[strings.ToLower(a.Login)] {
					finalAssignees = append(finalAssignees, a)
				}
			}
			assigneesJSON, _ := json.Marshal(finalAssignees)
			// Convert labels to JSONB (array of {name, color} objects)
			labelsJSON, _ := json.Marshal(it.Labels)

			// Parse date strings from GitHub API
			var createdAt, updatedAt, closedAt *time.Time
			if it.CreatedAt != nil && *it.CreatedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.CreatedAt); err == nil {
					createdAt = &t
				} else {
					slog.Warn("failed to parse issue created_at",
						"project_id", projectID,
						"repo", fullName,
						"issue_id", it.ID,
						"created_at", *it.CreatedAt,
						"error", err,
					)
				}
			}
			if it.UpdatedAt != nil && *it.UpdatedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.UpdatedAt); err == nil {
					updatedAt = &t
				} else {
					slog.Warn("failed to parse issue updated_at",
						"project_id", projectID,
						"repo", fullName,
						"issue_id", it.ID,
						"updated_at", *it.UpdatedAt,
						"error", err,
					)
				}
			}
			if it.ClosedAt != nil && *it.ClosedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.ClosedAt); err == nil {
					closedAt = &t
				} else {
					slog.Warn("failed to parse issue closed_at",
						"project_id", projectID,
						"repo", fullName,
						"issue_id", it.ID,
						"closed_at", *it.ClosedAt,
						"error", err,
					)
				}
			}

			// Fetch comments for this issue (if comments_count > 0, or if the
			// reconciliation step above just posted a new one - it.Comments
			// is the count from before that comment existed, so relying on
			// it alone here would let the bulk UPSERT below clobber the
			// comment just posted with the stale empty default).
			var commentsJSON []byte = []byte("[]")
			if it.Comments > 0 || commentPosted {
				if err := w.limiter.Wait(ctx); err == nil {
					comments, err := w.gh.ListIssueComments(ctx, token, fullName, it.Number)
					if err == nil {
						commentsJSON, _ = json.Marshal(comments)
					}
				}
			}

			_, _ = w.pool.Exec(ctx, `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, body, author_login, url, assignees, labels, comments_count, comments, created_at_github, updated_at_github, closed_at_github, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())
ON CONFLICT (project_id, github_issue_id) DO UPDATE SET
  number = EXCLUDED.number,
  state = EXCLUDED.state,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  author_login = EXCLUDED.author_login,
  url = EXCLUDED.url,
  assignees = EXCLUDED.assignees,
  labels = EXCLUDED.labels,
  comments_count = EXCLUDED.comments_count,
  comments = EXCLUDED.comments,
  created_at_github = COALESCE(EXCLUDED.created_at_github, github_issues.created_at_github),
  updated_at_github = COALESCE(EXCLUDED.updated_at_github, github_issues.updated_at_github),
  closed_at_github = COALESCE(EXCLUDED.closed_at_github, github_issues.closed_at_github),
  last_seen_at = now()
`, projectID, it.ID, it.Number, it.State, it.Title, it.Body, it.User.Login, it.HTMLURL, assigneesJSON, labelsJSON, it.Comments, commentsJSON, createdAt, updatedAt, closedAt)
		}
	}

	slog.Info("sync issues completed",
		"project_id", projectID,
		"repo", fullName,
		"total_issues", totalIssues,
	)
	return nil
}

// grainHackLabel is the GitHub label that marks an issue as part of the
// Grainlify hackathon - the only issues the assignment-eligibility
// reconciliation below is allowed to touch.
const grainHackLabel = "GrainHack"

// shouldEnforceAssignmentEligibility reports whether an issue is in scope
// for the auto-revert-ineligible-assignee reconciliation in syncIssues:
// open, and carrying the GrainHack label. Closed issues (nothing left to
// enforce - the work is done) and issues outside the hackathon must never
// be touched by this, no matter who's assigned to them.
// candidateLabels widens the check beyond the hardcoded literal above once a
// project has an accepted, active-hackathon application whose configured
// grainhack_label differs from it (internal/hackathon's Definitions - see
// label.go's EffectiveGrainHackLabels) - passing nil/empty falls back to
// checking only the legacy literal, so every existing caller/test keeps
// behaving exactly as before.
func shouldEnforceAssignmentEligibility(state string, labels []struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}, candidateLabels []string) bool {
	if !strings.EqualFold(state, "open") {
		return false
	}
	if len(candidateLabels) == 0 {
		candidateLabels = []string{grainHackLabel}
	}
	for _, l := range labels {
		for _, cl := range candidateLabels {
			if strings.EqualFold(l.Name, cl) {
				return true
			}
		}
	}
	return false
}

// reconcileApplicationStatuses keeps issue_applications in sync with
// GitHub's current assignee list for one issue, regardless of whether the
// assignment happened through Grainlify's own Assign()/Unassign() or
// directly on GitHub:
//   - an issue_applications row still 'assigned' whose login GitHub no
//     longer lists as an assignee is demoted back to 'applied'
//   - an eligible ('applied' or 'assigned') row whose login GitHub does
//     list, but that's still 'applied', is promoted to 'assigned'
//   - a GitHub assignee with no eligible row at all is reported back in
//     ineligible for the caller to remove via the GitHub API - Grainlify is
//     the source of truth for who may be assigned, so this is what makes
//     that enforceable even when the assignment bypassed the platform
//
// ghAssigneeLogins may be empty (GitHub reports no assignees at all) - that
// is exactly the case the demote step needs to handle, so this must be
// called unconditionally per issue, not skipped when there are no assignees.
func reconcileApplicationStatuses(ctx context.Context, pool db.DBPool, projectID uuid.UUID, issueNumber int, ghAssigneeLogins []string) (ineligible []string, err error) {
	lowered := make([]string, len(ghAssigneeLogins))
	for i, l := range ghAssigneeLogins {
		lowered[i] = strings.ToLower(l)
	}

	rows, err := pool.Query(ctx, `
SELECT LOWER(github_login)
FROM issue_applications
WHERE project_id = $1 AND issue_number = $2 AND status IN ('applied', 'assigned')
`, projectID, issueNumber)
	if err != nil {
		return nil, err
	}
	eligibleLogins := make(map[string]bool)
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			rows.Close()
			return nil, err
		}
		eligibleLogins[login] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	eligible := make([]string, 0, len(ghAssigneeLogins))
	for i, l := range lowered {
		if eligibleLogins[l] {
			eligible = append(eligible, l)
		} else {
			ineligible = append(ineligible, ghAssigneeLogins[i])
		}
	}

	if len(eligible) > 0 {
		if _, err := pool.Exec(ctx, `
UPDATE issue_applications
SET status = 'assigned', assigned_at = now(), updated_at = now()
WHERE project_id = $1 AND issue_number = $2 AND status = 'applied' AND LOWER(github_login) = ANY($3::text[])
`, projectID, issueNumber, eligible); err != nil {
			return ineligible, err
		}
	}

	if _, err := pool.Exec(ctx, `
UPDATE issue_applications
SET status = 'applied', assigned_at = NULL, updated_at = now()
WHERE project_id = $1 AND issue_number = $2 AND status = 'assigned' AND NOT (LOWER(github_login) = ANY($3::text[]))
`, projectID, issueNumber, lowered); err != nil {
		return ineligible, err
	}

	return ineligible, nil
}

func (w *Worker) syncPRs(ctx context.Context, projectID uuid.UUID, fullName string, token string) error {
	totalPRs := 0
	for page := 1; page <= 50; page++ { // safety cap
		if err := w.limiter.Wait(ctx); err != nil {
			return err
		}
		items, err := w.gh.ListPRsPage(ctx, token, fullName, page)
		if err != nil {
			slog.Error("failed to fetch PRs page",
				"project_id", projectID,
				"repo", fullName,
				"page", page,
				"error", err,
			)
			return err
		}
		if len(items) == 0 {
			slog.Info("sync PRs completed",
				"project_id", projectID,
				"repo", fullName,
				"total_prs", totalPRs,
			)
			return nil
		}

		for _, it := range items {
			totalPRs++

			// Parse date strings from GitHub API
			var createdAt, updatedAt, closedAt, mergedAt *time.Time
			if it.CreatedAt != nil && *it.CreatedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.CreatedAt); err == nil {
					createdAt = &t
				}
			}
			if it.UpdatedAt != nil && *it.UpdatedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.UpdatedAt); err == nil {
					updatedAt = &t
				}
			}
			if it.ClosedAt != nil && *it.ClosedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.ClosedAt); err == nil {
					closedAt = &t
				}
			}
			if it.MergedAt != nil && *it.MergedAt != "" {
				if t, err := time.Parse(time.RFC3339, *it.MergedAt); err == nil {
					mergedAt = &t
				}
			}

			// GitHub's "list pull requests" response has no `merged` field -
			// only `merged_at`. It is present on the single-PR "get" endpoint,
			// which this path does not call. So it.Merged unmarshalled to
			// false for every row this sync ever wrote: at the time of
			// writing, production held 1299 pull requests with a merge
			// timestamp and exactly zero with merged = true.
			//
			// Nothing read the column until merged PRs became the basis of
			// the leaderboard, which is how it stayed wrong this long. Derive
			// it from the timestamp that IS returned, and keep it.Merged as
			// the other half of the OR so the webhook path (which does set it
			// correctly) can't be regressed by a list sync arriving second.
			merged := it.Merged || mergedAt != nil

			_, _ = w.pool.Exec(ctx, `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, body, author_login, url, merged, created_at_github, updated_at_github, closed_at_github, merged_at_github, merge_commit_sha, head_sha, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now())
ON CONFLICT (project_id, github_pr_id) DO UPDATE SET
  number = EXCLUDED.number,
  state = EXCLUDED.state,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  author_login = EXCLUDED.author_login,
  url = EXCLUDED.url,
  -- Monotonic: a merged pull request cannot become unmerged on GitHub, so a
  -- later sync must never clear the flag. Without the OR, a list sync
  -- arriving after the merge webhook would overwrite a correct true with a
  -- derived value and silently drop the author's contribution.
  merged = github_pull_requests.merged OR EXCLUDED.merged,
  created_at_github = EXCLUDED.created_at_github,
  updated_at_github = EXCLUDED.updated_at_github,
  closed_at_github = EXCLUDED.closed_at_github,
  merge_commit_sha = COALESCE(EXCLUDED.merge_commit_sha, github_pull_requests.merge_commit_sha),
  head_sha = COALESCE(NULLIF(EXCLUDED.head_sha, ''), github_pull_requests.head_sha),
  merged_at_github = EXCLUDED.merged_at_github,
  last_seen_at = now()
`, projectID, it.ID, it.Number, it.State, it.Title, it.Body, it.User.Login, it.HTMLURL, merged, createdAt, updatedAt, closedAt, mergedAt, it.MergeCommitSHA, it.Head.SHA)
		}
	}

	// GrainHack judging intake (AI-specs.md §5). Reuses this sync path
	// rather than adding a webhook parser, same as §2.2's issue intake.
	// Runs after the PR rows are written, since it reads them back to find
	// merged PRs that name a GrainHack issue.
	//
	// Swallowed on failure: judging is downstream of the sync, and a
	// GrainHack problem must not fail a project's ordinary PR sync.
	if err := hackathon.SyncVerdicts(ctx, w.pool, w.gh, token, projectID, fullName); err != nil {
		slog.Warn("hackathon: verdict intake failed",
			"project_id", projectID, "repo", fullName, "error", err)
	}
	return nil
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown"
	}
	return h
}
