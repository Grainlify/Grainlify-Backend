package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type IssueApplicationsHandler struct {
	cfg    config.Config
	db     *db.DB
	notify *notifications.Service
}

func NewIssueApplicationsHandler(cfg config.Config, d *db.DB, notify *notifications.Service) *IssueApplicationsHandler {
	return &IssueApplicationsHandler{cfg: cfg, db: d, notify: notify}
}

// isGitHubInstallationNotFoundError reports whether err is GitHub responding
// that an installation access-token request failed because the installation
// itself no longer exists (app uninstalled, or the stored installation id is
// stale) - as opposed to a transient network/auth error. Distinguishing this
// matters because it's the one case with a clear, actionable fix (reinstall
// the GitHub App), unlike a generic installation_token_failed.
func isGitHubInstallationNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") || strings.Contains(s, "not found")
}

// recordApplication upserts the caller's issue_applications row to
// status='applied', creating it on a first application or reviving it after
// a prior withdrawal. commentID is nil-able since every other status
// transition below has no comment of its own to record.
func recordApplication(ctx context.Context, exec pgExecutor, userID, projectID uuid.UUID, issueNumber int, login string, commentID *int64) error {
	_, err := exec.Exec(ctx, `
INSERT INTO issue_applications (user_id, project_id, issue_number, github_login, github_comment_id, status, applied_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 'applied', now(), now())
ON CONFLICT (project_id, issue_number, user_id) DO UPDATE SET
  status = 'applied',
  github_login = EXCLUDED.github_login,
  github_comment_id = COALESCE(EXCLUDED.github_comment_id, issue_applications.github_comment_id),
  applied_at = now(),
  updated_at = now()
`, userID, projectID, issueNumber, login, commentID)
	return err
}

// recordAssignment upserts to status='assigned'. Upsert rather than
// update-only because a maintainer can Assign() a contributor who never
// called Apply() first.
func recordAssignment(ctx context.Context, exec pgExecutor, userID, projectID uuid.UUID, issueNumber int, login string) error {
	_, err := exec.Exec(ctx, `
INSERT INTO issue_applications (user_id, project_id, issue_number, github_login, status, assigned_at, updated_at)
VALUES ($1, $2, $3, $4, 'assigned', now(), now())
ON CONFLICT (project_id, issue_number, user_id) DO UPDATE SET
  status = 'assigned',
  github_login = EXCLUDED.github_login,
  assigned_at = now(),
  updated_at = now()
`, userID, projectID, issueNumber, login)
	return err
}

// recordRejection marks an existing application rejected. Update-only (not
// an upsert): rejecting a login with no application on file has nothing to
// persist.
func recordRejection(ctx context.Context, exec pgExecutor, projectID uuid.UUID, issueNumber int, login string) error {
	_, err := exec.Exec(ctx, `
UPDATE issue_applications SET status = 'rejected', resolved_at = now(), updated_at = now()
WHERE project_id = $1 AND issue_number = $2 AND LOWER(github_login) = LOWER($3)
`, projectID, issueNumber, login)
	return err
}

// recordWithdrawal marks the caller's own application withdrawn.
func recordWithdrawal(ctx context.Context, exec pgExecutor, userID, projectID uuid.UUID, issueNumber int) error {
	_, err := exec.Exec(ctx, `
UPDATE issue_applications SET status = 'withdrawn', resolved_at = now(), updated_at = now()
WHERE user_id = $1 AND project_id = $2 AND issue_number = $3
`, userID, projectID, issueNumber)
	return err
}

// recordUnassignment reverts a formerly-assigned application back to
// 'applied' (rather than deleting it) and clears assigned_at. Unassign()
// clears every assignee on the GitHub issue at once without knowing which
// specific applicant is being removed, so this mirrors that at the
// (project_id, issue_number) level rather than per-user.
func recordUnassignment(ctx context.Context, exec pgExecutor, projectID uuid.UUID, issueNumber int) error {
	_, err := exec.Exec(ctx, `
UPDATE issue_applications SET status = 'applied', assigned_at = NULL, updated_at = now()
WHERE project_id = $1 AND issue_number = $2 AND status = 'assigned'
`, projectID, issueNumber)
	return err
}

type applyToIssueRequest struct {
	Message string `json:"message"`
}

func (h *IssueApplicationsHandler) Apply() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.TokenEncKeyB64) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "token_encryption_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req applyToIssueRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "message_required"})
		}
		if len(req.Message) > 5000 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "message_too_long"})
		}

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "github_not_linked"})
		}

		// Load repo + issue state, issue URL, and github_issue_id for dashboard deep link.
		var fullName, issueURL string
		var state string
		var authorLogin string
		var assigneesJSON []byte
		var githubIssueID int64
		var ownerUserID uuid.UUID
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT p.github_full_name, gi.state, gi.author_login, gi.assignees, COALESCE(gi.url, ''), gi.github_issue_id, p.owner_user_id
FROM projects p
JOIN github_issues gi ON gi.project_id = p.id
WHERE p.id = $1 AND p.status = 'verified' AND p.deleted_at IS NULL
  AND gi.number = $2
LIMIT 1
`, projectID, issueNumber).Scan(&fullName, &state, &authorLogin, &assigneesJSON, &issueURL, &githubIssueID, &ownerUserID); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "issue_not_found"})
		}

		if strings.ToLower(strings.TrimSpace(state)) != "open" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "issue_not_open"})
		}
		if strings.EqualFold(strings.TrimSpace(authorLogin), strings.TrimSpace(linked.Login)) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "cannot_apply_to_own_issue"})
		}

		// "yet to be assigned" => no assignees.
		var assignees []any
		_ = json.Unmarshal(assigneesJSON, &assignees)
		if len(assignees) > 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "issue_already_assigned"})
		}

		// Build Drips Wave–style template: header, blockquote for message, maintainer instructions with links.
		quotedLines := strings.Split(req.Message, "\n")
		for i := range quotedLines {
			quotedLines[i] = "> " + quotedLines[i]
		}
		quotedMsg := strings.Join(quotedLines, "\n")
		// Deep link to this issue in the dashboard so "review their application" opens the exact issue.
		// The maintainer surface, not Browse - Browse is the contributor view
		// of the issue, where Reject/Assign/Unassign do not exist. Built from
		// the same one definition the in-app notification uses.
		reviewURL := notifications.AbsoluteLink(h.cfg.FrontendBaseURL,
			notifications.MaintainerApplicationLink(projectID.String(), githubIssueID))
		if issueURL == "" {
			issueURL = fmt.Sprintf("https://github.com/%s/issues/%d", fullName, issueNumber)
		}
		// The maintainer surface, not Browse: Browse renders the contributor
		// view of the issue, where Reject/Assign/Unassign do not exist.
		applicationLinkPath := notifications.MaintainerApplicationLink(projectID.String(), githubIssueID)
		commentBody := fmt.Sprintf("**📋 Grainlify Application**\n\n**@%s has applied to work on this issue as part of the Grainlify program.**\n\n%s\n\n---\n\n**Repo Maintainers:** To accept this application, [review their application](%s) or [assign @%s](%s) to this issue.",
			linked.Login, quotedMsg, reviewURL, linked.Login, issueURL)
		gh := github.NewClient()
		// Post as the applicant (user token) so the commenter is the user, not the bot (like Drips Wave: user + "with Drips Wave").
		ghComment, err := gh.CreateIssueComment(c.Context(), linked.AccessToken, fullName, issueNumber, commentBody)
		if err != nil {
			slog.Warn("failed to create github issue comment for application",
				"project_id", projectID.String(),
				"issue_number", issueNumber,
				"github_full_name", fullName,
				"user_id", userID.String(),
				"github_login", linked.Login,
				"error", err,
			)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_comment_create_failed"})
		}

		// Persist the comment into our DB so maintainers see it immediately.
		commentJSON, _ := json.Marshal(ghComment)
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues
SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
    comments_count = COALESCE(comments_count, 0) + 1,
    updated_at_github = $4,
    last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)

		if err := recordApplication(c.Context(), h.db.Pool, userID, projectID, issueNumber, linked.Login, &ghComment.ID); err != nil {
			slog.Error("issue_applications: record application failed", "error", err, "project_id", projectID, "issue_number", issueNumber)
		}

		h.notify.Notify(c.Context(), ownerUserID, notifications.TypeIssueApplicationSubmitted,
			fmt.Sprintf("New application from @%s", linked.Login),
			fmt.Sprintf("@%s applied to work on issue #%d in %s.", linked.Login, issueNumber, fullName),
			applicationLinkPath,
		)

		// And tell the applicant. This side was silent: applying notified the
		// maintainer and told the contributor nothing, so the first message
		// anybody ever received about their own application was its rejection.
		// 13 of the 14 people holding an open application had never had a
		// single notification about it.
		//
		// It matters more now than it did last week. Maintainers could not
		// reach their queue until the view gate and the notification link were
		// fixed, so almost nothing was ever resolved; as they start working
		// through it, refusals will land on people who were never told they
		// were being considered. A refusal arriving out of silence reads as a
		// system that was never listening.
		//
		// Deliberately says what happens next and does not promise a decision
		// by any particular time - nothing enforces one, and inventing a
		// deadline here would be the sort of claim that is only discovered to
		// be false by the person waiting on it.
		h.notify.Notify(c.Context(), userID, notifications.TypeIssueApplicationReceived,
			fmt.Sprintf("You applied to issue #%d", issueNumber),
			fmt.Sprintf("Your application for issue #%d in %s is with the maintainer. "+
				"You'll be notified when they decide, and you can withdraw it any time before then.",
				issueNumber, fullName),
			notifications.MyApplicationsLink(),
		)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok": true,
			"comment": fiber.Map{
				"id":         ghComment.ID,
				"body":       ghComment.Body,
				"user":       fiber.Map{"login": ghComment.User.Login},
				"created_at": ghComment.CreatedAt,
				"updated_at": ghComment.UpdatedAt,
			},
		})
	}
}

type botCommentRequest struct {
	Body string `json:"body"`
}

// PostBotComment posts a comment on a GitHub issue as the Grainlify GitHub App (bot).
// Requires project maintainer (owner) or admin. Project must have GitHub App installed.
func (h *IssueApplicationsHandler) PostBotComment() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "github_app_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req botCommentRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		req.Body = strings.TrimSpace(req.Body)
		if req.Body == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "body_required"})
		}
		if len(req.Body) > 32000 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "body_too_long"})
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "project_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project_lookup_failed"})
		}
		allowed, err := ownerOrLiveAdmin(c.Context(), h.db, owner, userID)
		if err != nil {
			slog.Error("owner-or-admin check", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authz_check_failed"})
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if installationID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_has_no_github_app_installation"})
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for bot comment", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "github_app_client_failed"})
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for bot comment",
				"project_id", projectID.String(),
				"installation_id", installationID,
				"error", err,
			)
			if isGitHubInstallationNotFoundError(err) {
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_installation_not_found"})
			}
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "installation_token_failed"})
		}

		gh := github.NewClient()
		ghComment, err := gh.CreateIssueComment(c.Context(), token, fullName, issueNumber, req.Body)
		if err != nil {
			slog.Warn("failed to post bot comment on GitHub",
				"project_id", projectID.String(),
				"issue_number", issueNumber,
				"github_full_name", fullName,
				"error", err,
			)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_comment_create_failed"})
		}

		commentJSON, _ := json.Marshal(ghComment)
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues
SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
    comments_count = COALESCE(comments_count, 0) + 1,
    updated_at_github = $4,
    last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok": true,
			"comment": fiber.Map{
				"id":         ghComment.ID,
				"body":       ghComment.Body,
				"user":       fiber.Map{"login": ghComment.User.Login},
				"created_at": ghComment.CreatedAt,
				"updated_at": ghComment.UpdatedAt,
			},
		})
	}
}

type withdrawRequest struct {
	CommentID int64 `json:"comment_id"`
}

// Withdraw removes the applicant's application by deleting their GitHub comment. Only the comment author can withdraw.
func (h *IssueApplicationsHandler) Withdraw() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.TokenEncKeyB64) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "token_encryption_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req withdrawRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if req.CommentID <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "comment_id_required"})
		}

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "github_not_linked"})
		}

		var fullName string
		var commentsJSON []byte
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT p.github_full_name, COALESCE(gi.comments, '[]'::jsonb)
FROM projects p
JOIN github_issues gi ON gi.project_id = p.id
WHERE p.id = $1 AND p.status = 'verified' AND p.deleted_at IS NULL AND gi.number = $2
`, projectID, issueNumber).Scan(&fullName, &commentsJSON); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "issue_not_found"})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project_lookup_failed"})
		}

		// Verify the comment exists and belongs to the current user before calling GitHub (avoids 403/502)
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(commentsJSON, &comments); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "comments_parse_failed"})
		}
		var commentOwned bool
		for _, com := range comments {
			if com.ID == req.CommentID {
				if !strings.EqualFold(strings.TrimSpace(com.User.Login), strings.TrimSpace(linked.Login)) {
					return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "you_can_only_withdraw_your_own_application"})
				}
				commentOwned = true
				break
			}
		}
		if !commentOwned {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "comment_not_found"})
		}

		gh := github.NewClient()
		if err := gh.DeleteIssueComment(c.Context(), linked.AccessToken, fullName, req.CommentID); err != nil {
			var ghErr *github.GitHubAPIError
			if errors.As(err, &ghErr) {
				if ghErr.StatusCode == 403 {
					return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "cannot_delete_comment_forbidden"})
				}
				if ghErr.StatusCode == 404 {
					return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "comment_not_found"})
				}
			}
			slog.Warn("failed to delete github comment for withdraw",
				"project_id", projectID.String(), "issue_number", issueNumber, "comment_id", req.CommentID,
				"user_id", userID.String(), "error", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_comment_delete_failed"})
		}

		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues
SET comments = (
  SELECT COALESCE(jsonb_agg(elem), '[]'::jsonb)
  FROM jsonb_array_elements(COALESCE(comments, '[]'::jsonb)) AS elem
  WHERE (elem->>'id')::bigint != $3
),
comments_count = GREATEST(0, COALESCE(comments_count, 0) - 1),
last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, req.CommentID)

		if err := recordWithdrawal(c.Context(), h.db.Pool, userID, projectID, issueNumber); err != nil {
			slog.Error("issue_applications: record withdrawal failed", "error", err, "project_id", projectID, "issue_number", issueNumber)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type assignRequest struct {
	Assignee string `json:"assignee"`
}

// Assign adds the applicant as assignee on GitHub and posts a congratulations bot comment. Maintainer only.
func (h *IssueApplicationsHandler) Assign() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "github_app_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req assignRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		req.Assignee = strings.TrimSpace(req.Assignee)
		if req.Assignee == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "assignee_required"})
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "project_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project_lookup_failed"})
		}
		allowed, err := ownerOrLiveAdmin(c.Context(), h.db, owner, userID)
		if err != nil {
			slog.Error("owner-or-admin check", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authz_check_failed"})
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if installationID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_has_no_github_app_installation"})
		}

		var alreadyAssigned bool
		_ = h.db.Pool.QueryRow(c.Context(), `
SELECT EXISTS(SELECT 1 FROM issue_applications WHERE project_id = $1 AND issue_number = $2 AND status = 'assigned')
`, projectID, issueNumber).Scan(&alreadyAssigned)
		if alreadyAssigned {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "issue_already_assigned"})
		}

		var applicantStatus string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT status FROM issue_applications
WHERE project_id = $1 AND issue_number = $2 AND LOWER(github_login) = LOWER($3)
`, projectID, issueNumber, req.Assignee).Scan(&applicantStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "applicant_lookup_failed"})
		}
		if applicantStatus != "applied" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "assignee_has_not_applied"})
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for assign", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "github_app_client_failed"})
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for assign", "project_id", projectID.String(), "error", err)
			if isGitHubInstallationNotFoundError(err) {
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_installation_not_found"})
			}
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "installation_token_failed"})
		}

		gh := github.NewClient()
		if err := gh.AddIssueAssignees(c.Context(), token, fullName, issueNumber, []string{req.Assignee}); err != nil {
			slog.Warn("failed to add assignee on GitHub", "project_id", projectID.String(), "issue_number", issueNumber, "assignee", req.Assignee, "error", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_assign_failed"})
		}

		assigneesJSON, _ := json.Marshal([]map[string]string{{"login": req.Assignee}})
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET assignees = $3, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, assigneesJSON)

		var githubIssueID int64
		_ = h.db.Pool.QueryRow(c.Context(), `SELECT github_issue_id FROM github_issues WHERE project_id = $1 AND number = $2`, projectID, issueNumber).Scan(&githubIssueID)
		// Addressed to maintainers ("You can manage this issue"), so it points
		// at the maintainer surface rather than the contributor view of it.
		manageURL := notifications.AbsoluteLink(h.cfg.FrontendBaseURL,
			notifications.MaintainerApplicationLink(projectID.String(), githubIssueID))
		botBody := fmt.Sprintf("Congratulations, **@%s**! 🎉 Your application was accepted by the repo's maintainers.\n\n"+
			"Please resolve the issue such that the repo's maintainers have enough time to review your contribution.\n\n"+
			"> ⚠️ **Warning:** When opening a PR, please link it to this issue to ensure it gets tracked accurately.\n\n"+
			"**Repo maintainers:** You can manage this issue, including adjusting complexity and points, [here](%s).",
			req.Assignee, manageURL)

		ghComment, err := gh.CreateIssueComment(c.Context(), token, fullName, issueNumber, botBody)
		if err != nil {
			slog.Warn("assign: bot congratulations comment failed", "error", err)
		} else {
			commentJSON, _ := json.Marshal(ghComment)
			_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
  comments_count = COALESCE(comments_count, 0) + 1, updated_at_github = $4, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)
		}

		if assigneeUserID, ok := notifications.ResolveUserIDByGitHubLogin(c.Context(), h.db, req.Assignee); ok {
			if err := recordAssignment(c.Context(), h.db.Pool, assigneeUserID, projectID, issueNumber, req.Assignee); err != nil {
				slog.Error("issue_applications: record assignment failed", "error", err, "project_id", projectID, "issue_number", issueNumber)
			}
			h.notify.Notify(c.Context(), assigneeUserID, notifications.TypeIssueAssigned,
				fmt.Sprintf("You've been assigned to issue #%d", issueNumber),
				fmt.Sprintf("You were assigned to work on issue #%d in %s.", issueNumber, fullName),
				notifications.IssueLink(projectID.String(), githubIssueID),
			)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// Unassign removes the current assignee(s) from the GitHub issue and posts a bot comment. Maintainer only.
func (h *IssueApplicationsHandler) Unassign() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "github_app_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var owner uuid.UUID
		var fullName, installationID string
		var assigneesJSON []byte
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT p.owner_user_id, p.github_full_name, COALESCE(p.github_app_installation_id, ''), COALESCE(gi.assignees, '[]'::jsonb)
FROM projects p
JOIN github_issues gi ON gi.project_id = p.id
WHERE p.id = $1 AND p.status = 'verified' AND p.deleted_at IS NULL AND gi.number = $2
`, projectID, issueNumber).Scan(&owner, &fullName, &installationID, &assigneesJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "issue_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project_lookup_failed"})
		}
		allowed, err := ownerOrLiveAdmin(c.Context(), h.db, owner, userID)
		if err != nil {
			slog.Error("owner-or-admin check", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authz_check_failed"})
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if installationID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_has_no_github_app_installation"})
		}

		var assignees []struct {
			Login string `json:"login"`
		}
		_ = json.Unmarshal(assigneesJSON, &assignees)
		if len(assignees) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "issue_has_no_assignees"})
		}
		logins := make([]string, 0, len(assignees))
		for _, a := range assignees {
			if a.Login != "" {
				logins = append(logins, a.Login)
			}
		}
		if len(logins) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "issue_has_no_assignees"})
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for unassign", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "github_app_client_failed"})
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for unassign", "project_id", projectID.String(), "error", err)
			if isGitHubInstallationNotFoundError(err) {
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_installation_not_found"})
			}
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "installation_token_failed"})
		}

		gh := github.NewClient()
		if err := gh.RemoveIssueAssignees(c.Context(), token, fullName, issueNumber, logins); err != nil {
			slog.Warn("failed to remove assignees on GitHub", "project_id", projectID.String(), "issue_number", issueNumber, "error", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_unassign_failed"})
		}

		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET assignees = '[]'::jsonb, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber)

		if err := recordUnassignment(c.Context(), h.db.Pool, projectID, issueNumber); err != nil {
			slog.Error("issue_applications: record unassignment failed", "error", err, "project_id", projectID, "issue_number", issueNumber)
		}

		who := "@" + logins[0]
		if len(logins) > 1 {
			who = "@" + strings.Join(logins, ", @")
		}
		botBody := fmt.Sprintf("%s has been unassigned from this issue. The maintainer may assign another contributor.", who)

		ghComment, err := gh.CreateIssueComment(c.Context(), token, fullName, issueNumber, botBody)
		if err != nil {
			slog.Warn("unassign: bot comment failed", "error", err)
		} else {
			commentJSON, _ := json.Marshal(ghComment)
			_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
  comments_count = COALESCE(comments_count, 0) + 1, updated_at_github = $4, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type rejectRequest struct {
	Assignee string `json:"assignee"`
}

// Reject posts a bot comment that the applicant's application was not accepted. Maintainer only.
func (h *IssueApplicationsHandler) Reject() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "github_app_not_configured"})
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req rejectRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		req.Assignee = strings.TrimSpace(req.Assignee)
		if req.Assignee == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "assignee_required"})
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "project_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project_lookup_failed"})
		}
		allowed, err := ownerOrLiveAdmin(c.Context(), h.db, owner, userID)
		if err != nil {
			slog.Error("owner-or-admin check", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authz_check_failed"})
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}
		if installationID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_has_no_github_app_installation"})
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for reject", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "github_app_client_failed"})
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for reject", "project_id", projectID.String(), "error", err)
			if isGitHubInstallationNotFoundError(err) {
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_installation_not_found"})
			}
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "installation_token_failed"})
		}

		botBody := fmt.Sprintf("@%s your application was not accepted for this issue. The maintainer may assign another contributor.", req.Assignee)
		gh := github.NewClient()
		ghComment, err := gh.CreateIssueComment(c.Context(), token, fullName, issueNumber, botBody)
		if err != nil {
			slog.Warn("reject: bot comment failed", "error", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "github_comment_create_failed"})
		}
		commentJSON, _ := json.Marshal(ghComment)
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
  comments_count = COALESCE(comments_count, 0) + 1, updated_at_github = $4, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)

		if err := recordRejection(c.Context(), h.db.Pool, projectID, issueNumber, req.Assignee); err != nil {
			slog.Error("issue_applications: record rejection failed", "error", err, "project_id", projectID, "issue_number", issueNumber)
		}

		if applicantUserID, ok := notifications.ResolveUserIDByGitHubLogin(c.Context(), h.db, req.Assignee); ok {
			// Built, not hand-written. This was its own copy of IssueLink's
			// format string - identical today, and a second definition of the
			// same rule, which is how two spellings of a settings path reached
			// production and how a maintainer link came to point at the
			// contributor view.
			//
			// It points at the issue rather than at their applications board,
			// because the board deliberately does not carry refusals: a
			// contributor should not have to keep looking at a list of the
			// things they were turned down for. The issue is still open and
			// still real, which is the useful thing left to show them.
			var githubIssueID int64
			_ = h.db.Pool.QueryRow(c.Context(), `SELECT github_issue_id FROM github_issues WHERE project_id = $1 AND number = $2`, projectID, issueNumber).Scan(&githubIssueID)
			h.notify.Notify(c.Context(), applicantUserID, notifications.TypeIssueApplicationRejected,
				fmt.Sprintf("Application not accepted for issue #%d", issueNumber),
				fmt.Sprintf("Your application for issue #%d in %s was not accepted this time. "+
					"The issue may still be open, and applying to others does not count against you.",
					issueNumber, fullName),
				notifications.IssueLink(projectID.String(), githubIssueID),
			)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type issueApplicationDTO struct {
	ID          uuid.UUID  `json:"id"`
	Status      string     `json:"status"` // applied | assigned | pending_review | complete
	ProjectID   uuid.UUID  `json:"project_id"`
	ProjectName string     `json:"project_name"`
	IssueNumber int        `json:"issue_number"`
	IssueTitle  string     `json:"issue_title"`
	IssueURL    string     `json:"issue_url"`
	Labels      []string   `json:"labels"`
	AppliedAt   *time.Time `json:"applied_at,omitempty"`
	AssignedAt  *time.Time `json:"assigned_at,omitempty"`
	PRNumber    *int       `json:"pr_number,omitempty"`
	PRURL       *string    `json:"pr_url,omitempty"`
	PRTitle     *string    `json:"pr_title,omitempty"`
	PRCreatedAt *time.Time `json:"pr_created_at,omitempty"`
	PRMergedAt  *time.Time `json:"pr_merged_at,omitempty"`
}

// issueLabelNames extracts label names from a github_issues.labels JSONB
// value, shaped [{"name": "...", "color": "..."}] per internal/github/list.go.
func issueLabelNames(labelsJSON []byte) []string {
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(labelsJSON, &labels); err != nil {
		return []string{}
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			names = append(names, l.Name)
		}
	}
	return names
}

// Mine handles GET /issue-applications/me: the caller's own issue
// applications, bucketed into applied/assigned/pending_review/complete.
// pending_review/complete are derived at read time, not stored - an assigned
// application is matched against github_pull_requests.body for a GitHub
// closing keyword ("fixes #12"/"closes #12"/"resolves #12") referencing this
// issue, rather than a persisted PR<->issue link (see migration 000032).
func (h *IssueApplicationsHandler) Mine() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT
  ia.id, ia.status, ia.applied_at, ia.assigned_at,
  gi.number, gi.title, COALESCE(gi.url, ''), COALESCE(gi.labels, '[]'::jsonb),
  p.id, p.github_full_name,
  pr.number, pr.url, pr.title, pr.merged, pr.created_at_github, pr.merged_at_github
FROM issue_applications ia
INNER JOIN github_issues gi ON gi.project_id = ia.project_id AND gi.number = ia.issue_number
INNER JOIN projects p ON p.id = ia.project_id
LEFT JOIN LATERAL (
  SELECT pr.number, pr.url, pr.title, pr.merged, pr.created_at_github, pr.merged_at_github
  FROM github_pull_requests pr
  WHERE pr.project_id = ia.project_id
    AND LOWER(pr.author_login) = LOWER(ia.github_login)
    -- GitHub only auto-closes an issue when its own keyword+number appears
    -- ("Closes #12, #34" does not link #34 without its own keyword), so this
    -- intentionally requires the same per-number pairing rather than
    -- matching any issue number mentioned anywhere in the body.
    AND pr.body ~* ('(^|[^0-9A-Za-z])(close[sd]?|fix(e[sd])?|resolve[sd]?)[[:space:]]*:?[[:space:]]*#' || ia.issue_number::text || '([^0-9]|$)')
  ORDER BY pr.created_at_github DESC NULLS LAST
  LIMIT 1
) pr ON ia.status = 'assigned'
WHERE ia.user_id = $1 AND ia.status IN ('applied', 'assigned')
ORDER BY ia.created_at DESC
LIMIT 200
`, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issue_applications_list_failed"})
		}
		defer rows.Close()

		out := []issueApplicationDTO{}
		for rows.Next() {
			var (
				d          issueApplicationDTO
				dbStatus   string
				labelsJSON []byte
				prMerged   *bool
			)
			if err := rows.Scan(
				&d.ID, &dbStatus, &d.AppliedAt, &d.AssignedAt,
				&d.IssueNumber, &d.IssueTitle, &d.IssueURL, &labelsJSON,
				&d.ProjectID, &d.ProjectName,
				&d.PRNumber, &d.PRURL, &d.PRTitle, &prMerged, &d.PRCreatedAt, &d.PRMergedAt,
			); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issue_applications_scan_failed"})
			}

			d.Labels = issueLabelNames(labelsJSON)

			switch {
			case dbStatus == "applied":
				d.Status = "applied"
			case d.PRNumber == nil:
				d.Status = "assigned"
			case prMerged != nil && *prMerged:
				d.Status = "complete"
			default:
				d.Status = "pending_review"
			}

			out = append(out, d)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"issue_applications": out})
	}
}
