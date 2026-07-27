package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

type IssueApplicationsHandler struct {
	cfg config.Config
	db  *db.DB
}

func NewIssueApplicationsHandler(cfg config.Config, d *db.DB) *IssueApplicationsHandler {
	return &IssueApplicationsHandler{cfg: cfg, db: d}
}

type applyToIssueRequest struct {
	Message string `json:"message"`
}

func (h *IssueApplicationsHandler) Apply() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Validate the idempotency key format before anything else so a malformed
		// header is reported even when downstream dependencies aren't configured.
		idempotencyKey := strings.TrimSpace(c.Get("Idempotency-Key"))
		if idempotencyKey != "" && len(idempotencyKey) > 255 {
			// Max 255 characters to fit in the TEXT column and prevent abuse.
			return httpx.RespondError(c, fiber.StatusBadRequest, "idempotency_key_too_long", "Idempotency-Key header must be 255 characters or less")
		}

		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.TokenEncKeyB64) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "token_encryption_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		// Idempotency key support: if Idempotency-Key header is present, check for a cached response.
		// The key is scoped per-user to prevent cross-user response leaks.
		if idempotencyKey != "" {
			// Query idempotency_keys for a cached response. Always include user_id in the WHERE clause
			// to enforce per-user scoping — never query by idempotency_key alone.
			var responseStatus int
			var responseBody string
			err := h.db.Pool.QueryRow(c.Context(), `
SELECT response_status, response_body
FROM idempotency_keys
WHERE user_id = $1 AND idempotency_key = $2 AND expires_at > now()
LIMIT 1
`, userID, idempotencyKey).Scan(&responseStatus, &responseBody)

			if err == nil {
				// Cache hit: return the stored response without executing the application creation logic.
				slog.Info("idempotency cache hit",
					"user_id", userID.String(),
					"idempotency_key_hash", fmt.Sprintf("%x", hashString(idempotencyKey)[:8]), // Log truncated hash, not the key itself
					"project_id", projectID.String(),
					"issue_number", issueNumber,
				)
				// Deserialize the stored JSON response body and return it with the original status code.
				var cachedResponse fiber.Map
				if jsonErr := json.Unmarshal([]byte(responseBody), &cachedResponse); jsonErr != nil {
					slog.Error("failed to deserialize cached idempotency response",
						"user_id", userID.String(),
						"error", jsonErr,
					)
					// If deserialization fails, fall through to re-execute the request rather than failing.
				} else {
					return c.Status(responseStatus).JSON(cachedResponse)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				// Log unexpected database errors but do not fail the request — fall through to execute normally.
				slog.Warn("idempotency key lookup failed",
					"user_id", userID.String(),
					"error", err,
				)
			}
			// Cache miss or lookup error: proceed to normal application creation logic.
		}

		var req applyToIssueRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_body", "")
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "message_required", "")
		}
		if len(req.Message) > 5000 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "message_too_long", "")
		}

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "github_not_linked", "")
		}

		// Load repo + issue state, issue URL, and github_issue_id for dashboard deep link.
		var fullName, issueURL string
		var state string
		var authorLogin string
		var assigneesJSON []byte
		var githubIssueID int64
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT p.github_full_name, gi.state, gi.author_login, gi.assignees, COALESCE(gi.url, ''), gi.github_issue_id
FROM projects p
JOIN github_issues gi ON gi.project_id = p.id
WHERE p.id = $1 AND p.status = 'verified' AND p.deleted_at IS NULL
  AND gi.number = $2
LIMIT 1
`, projectID, issueNumber).Scan(&fullName, &state, &authorLogin, &assigneesJSON, &issueURL, &githubIssueID); err != nil {
			return httpx.RespondError(c, fiber.StatusNotFound, "issue_not_found", "")
		}

		if strings.ToLower(strings.TrimSpace(state)) != "open" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "issue_not_open", "")
		}
		if strings.EqualFold(strings.TrimSpace(authorLogin), strings.TrimSpace(linked.Login)) {
			return httpx.RespondError(c, fiber.StatusBadRequest, "cannot_apply_to_own_issue", "")
		}

		// "yet to be assigned" => no assignees.
		var assignees []any
		_ = json.Unmarshal(assigneesJSON, &assignees)
		if len(assignees) > 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "issue_already_assigned", "")
		}

		// Build Drips Wave–style template: header, blockquote for message, maintainer instructions with links.
		quotedLines := strings.Split(req.Message, "\n")
		for i := range quotedLines {
			quotedLines[i] = "> " + quotedLines[i]
		}
		quotedMsg := strings.Join(quotedLines, "\n")
		// Deep link to this issue in the dashboard so "review their application" opens the exact issue.
		base := strings.TrimSpace(strings.TrimRight(h.cfg.FrontendBaseURL, "/"))
		reviewURL := fmt.Sprintf("%s/dashboard?tab=browse&project=%s&issue=%d", base, projectID.String(), githubIssueID)
		if base == "" || !strings.HasPrefix(base, "http") {
			// Fallback: relative path only if FrontendBaseURL not configured (link will use current origin)
			reviewURL = fmt.Sprintf("/dashboard?tab=browse&project=%s&issue=%d", projectID.String(), githubIssueID)
		}
		if issueURL == "" {
			issueURL = fmt.Sprintf("https://github.com/%s/issues/%d", fullName, issueNumber)
		}
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
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_comment_create_failed", "")
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

		// Build success response.
		successResponse := fiber.Map{
			"ok": true,
			"comment": fiber.Map{
				"id":         ghComment.ID,
				"body":       ghComment.Body,
				"user":       fiber.Map{"login": ghComment.User.Login},
				"created_at": ghComment.CreatedAt,
				"updated_at": ghComment.UpdatedAt,
			},
		}

		// If an idempotency key was provided, cache this success response for 24 hours.
		// Cache writes are best-effort: if the write fails, log the error but do not fail
		// the request — the application was successfully created and the response has already
		// been committed to the client.
		if idempotencyKey != "" {
			responseBodyJSON, _ := json.Marshal(successResponse)
			_, insertErr := h.db.Pool.Exec(c.Context(), `
INSERT INTO idempotency_keys (user_id, idempotency_key, response_status, response_body, created_at, expires_at)
VALUES ($1, $2, $3, $4, now(), now() + interval '24 hours')
ON CONFLICT (user_id, idempotency_key) DO NOTHING
`, userID, idempotencyKey, fiber.StatusOK, string(responseBodyJSON))
			if insertErr != nil {
				// Log the error but do not fail the request — the application was created successfully.
				slog.Warn("failed to write idempotency cache",
					"user_id", userID.String(),
					"project_id", projectID.String(),
					"issue_number", issueNumber,
					"error", insertErr,
				)
			}
		}

		return c.Status(fiber.StatusOK).JSON(successResponse)
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
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "github_app_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}
		role, _ := c.Locals(auth.LocalRole).(string)

		var req botCommentRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_body", "")
		}
		req.Body = strings.TrimSpace(req.Body)
		if req.Body == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "body_required", "")
		}
		if len(req.Body) > 32000 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "body_too_long", "")
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}
		if installationID == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "project_has_no_github_app_installation", "")
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for bot comment", "error", err)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "github_app_client_failed", "")
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for bot comment",
				"project_id", projectID.String(),
				"installation_id", installationID,
				"error", err,
			)
			return httpx.RespondError(c, fiber.StatusBadGateway, "installation_token_failed", "")
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
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_comment_create_failed", "")
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
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.TokenEncKeyB64) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "token_encryption_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		var req withdrawRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_body", "")
		}
		if req.CommentID <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "comment_id_required", "")
		}

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "github_not_linked", "")
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
				return httpx.RespondError(c, fiber.StatusNotFound, "issue_not_found", "")
			}
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
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
			return httpx.RespondError(c, fiber.StatusInternalServerError, "comments_parse_failed", "")
		}
		var commentOwned bool
		for _, com := range comments {
			if com.ID == req.CommentID {
				if !strings.EqualFold(strings.TrimSpace(com.User.Login), strings.TrimSpace(linked.Login)) {
					return httpx.RespondError(c, fiber.StatusForbidden, "you_can_only_withdraw_your_own_application", "")
				}
				commentOwned = true
				break
			}
		}
		if !commentOwned {
			return httpx.RespondError(c, fiber.StatusNotFound, "comment_not_found", "")
		}

		gh := github.NewClient()
		if err := gh.DeleteIssueComment(c.Context(), linked.AccessToken, fullName, req.CommentID); err != nil {
			var ghErr *github.GitHubAPIError
			if errors.As(err, &ghErr) {
				if ghErr.StatusCode == 403 {
					return httpx.RespondError(c, fiber.StatusForbidden, "cannot_delete_comment_forbidden", "")
				}
				if ghErr.StatusCode == 404 {
					return httpx.RespondError(c, fiber.StatusNotFound, "comment_not_found", "")
				}
			}
			slog.Warn("failed to delete github comment for withdraw",
				"project_id", projectID.String(), "issue_number", issueNumber, "comment_id", req.CommentID,
				"user_id", userID.String(), "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_comment_delete_failed", "")
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
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "github_app_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}
		role, _ := c.Locals(auth.LocalRole).(string)

		var req assignRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_body", "")
		}
		req.Assignee = strings.TrimSpace(req.Assignee)
		if req.Assignee == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "assignee_required", "")
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}
		if installationID == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "project_has_no_github_app_installation", "")
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for assign", "error", err)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "github_app_client_failed", "")
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for assign", "project_id", projectID.String(), "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "installation_token_failed", "")
		}

		gh := github.NewClient()
		if err := gh.AddIssueAssignees(c.Context(), token, fullName, issueNumber, []string{req.Assignee}); err != nil {
			slog.Warn("failed to add assignee on GitHub", "project_id", projectID.String(), "issue_number", issueNumber, "assignee", req.Assignee, "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_assign_failed", "")
		}

		assigneesJSON, _ := json.Marshal([]map[string]string{{"login": req.Assignee}})
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET assignees = $3, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, assigneesJSON)

		var githubIssueID int64
		_ = h.db.Pool.QueryRow(c.Context(), `SELECT github_issue_id FROM github_issues WHERE project_id = $1 AND number = $2`, projectID, issueNumber).Scan(&githubIssueID)
		base := strings.TrimSpace(strings.TrimRight(h.cfg.FrontendBaseURL, "/"))
		manageURL := base + "/dashboard?tab=browse&project=" + projectID.String() + "&issue=" + fmt.Sprintf("%d", githubIssueID)
		if base == "" || !strings.HasPrefix(base, "http") {
			manageURL = "/dashboard?tab=browse&project=" + projectID.String() + "&issue=" + fmt.Sprintf("%d", githubIssueID)
		}
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

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// Unassign removes the current assignee(s) from the GitHub issue and posts a bot comment. Maintainer only.
func (h *IssueApplicationsHandler) Unassign() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "github_app_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}
		role, _ := c.Locals(auth.LocalRole).(string)

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
			return httpx.RespondError(c, fiber.StatusNotFound, "issue_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}
		if installationID == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "project_has_no_github_app_installation", "")
		}

		var assignees []struct {
			Login string `json:"login"`
		}
		_ = json.Unmarshal(assigneesJSON, &assignees)
		if len(assignees) == 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "issue_has_no_assignees", "")
		}
		logins := make([]string, 0, len(assignees))
		for _, a := range assignees {
			if a.Login != "" {
				logins = append(logins, a.Login)
			}
		}
		if len(logins) == 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "issue_has_no_assignees", "")
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for unassign", "error", err)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "github_app_client_failed", "")
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for unassign", "project_id", projectID.String(), "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "installation_token_failed", "")
		}

		gh := github.NewClient()
		if err := gh.RemoveIssueAssignees(c.Context(), token, fullName, issueNumber, logins); err != nil {
			slog.Warn("failed to remove assignees on GitHub", "project_id", projectID.String(), "issue_number", issueNumber, "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_unassign_failed", "")
		}

		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET assignees = '[]'::jsonb, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber)

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
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "github_app_not_configured", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil || issueNumber <= 0 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_issue_number", "")
		}

		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}
		role, _ := c.Locals(auth.LocalRole).(string)

		var req rejectRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_body", "")
		}
		req.Assignee = strings.TrimSpace(req.Assignee)
		if req.Assignee == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "assignee_required", "")
		}

		var owner uuid.UUID
		var fullName, installationID string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL
`, projectID).Scan(&owner, &fullName, &installationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}
		if installationID == "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, "project_has_no_github_app_installation", "")
		}

		appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("failed to create GitHub App client for reject", "error", err)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "github_app_client_failed", "")
		}
		token, err := appClient.GetInstallationToken(c.Context(), installationID)
		if err != nil {
			slog.Warn("failed to get installation token for reject", "project_id", projectID.String(), "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "installation_token_failed", "")
		}

		botBody := fmt.Sprintf("@%s your application was not accepted for this issue. The maintainer may assign another contributor.", req.Assignee)
		gh := github.NewClient()
		ghComment, err := gh.CreateIssueComment(c.Context(), token, fullName, issueNumber, botBody)
		if err != nil {
			slog.Warn("reject: bot comment failed", "error", err)
			return httpx.RespondError(c, fiber.StatusBadGateway, "github_comment_create_failed", "")
		}
		commentJSON, _ := json.Marshal(ghComment)
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE github_issues SET comments = COALESCE(comments, '[]'::jsonb) || $3::jsonb,
  comments_count = COALESCE(comments_count, 0) + 1, updated_at_github = $4, last_seen_at = now()
WHERE project_id = $1 AND number = $2
`, projectID, issueNumber, commentJSON, ghComment.UpdatedAt)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// hashString returns the SHA-256 hash of a string for logging purposes.
// Used to log a non-sensitive reference to idempotency keys without exposing the full key value.
func hashString(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
