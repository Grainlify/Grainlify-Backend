package handlers

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

type ProjectDataHandler struct {
	db *db.DB
}

var errProjectDataResponseWritten = errors.New("project data response already written")

func respondProjectDataError(c *fiber.Ctx, status int, code httpx.Code) error {
	_ = httpx.RespondError(c, status, code, "")
	return errProjectDataResponseWritten
}

func NewProjectDataHandler(d *db.DB) *ProjectDataHandler {
	return &ProjectDataHandler{db: d}
}

// projectIDForRead returns project ID if the user is authenticated and the project exists (verified).
// Any authenticated user can read project issues/PRs/events (e.g. contributors browsing issues).
func (h *ProjectDataHandler) projectIDForRead(c *fiber.Ctx) (uuid.UUID, error) {
	if h.db == nil || h.db.Pool == nil {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusServiceUnavailable, "db_not_configured")
	}
	if _, ok := c.Locals(auth.LocalUserID).(string); !ok {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusUnauthorized, "invalid_user")
	}
	projectID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusBadRequest, "invalid_project_id")
	}
	var exists bool
	err = h.db.Pool.QueryRow(c.Context(), `
SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1 AND status = 'verified' AND deleted_at IS NULL)
`, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusNotFound, "project_not_found")
	}
	if err != nil {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusInternalServerError, "project_lookup_failed")
	}
	if !exists {
		return uuid.Nil, respondProjectDataError(c, fiber.StatusNotFound, "project_not_found")
	}
	return projectID, nil
}

func (h *ProjectDataHandler) Issues() fiber.Handler {
	return func(c *fiber.Ctx) error {
		projectID, err := h.projectIDForRead(c)
		if err != nil {
			return nil
		}

		p, err := ParsePagination(c, 20, 100)
		if err != nil {
			// response already written by ParsePagination on error
			return nil
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT github_issue_id, number, state, title, body, author_login, url, assignees, labels, comments_count, comments, updated_at_github, last_seen_at
FROM github_issues
WHERE project_id = $1
ORDER BY COALESCE(updated_at_github, last_seen_at) DESC
LIMIT $2 OFFSET $3
`, projectID, p.Limit, p.Offset)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "issues_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var gid int64
			var number int
			var state, title, author, url string
			var body *string
			var assigneesJSON, labelsJSON, commentsJSON []byte
			var commentsCount int
			var updated *time.Time
			var lastSeen time.Time
			if err := rows.Scan(&gid, &number, &state, &title, &body, &author, &url, &assigneesJSON, &labelsJSON, &commentsCount, &commentsJSON, &updated, &lastSeen); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "issues_list_failed", "")
			}

			var assignees []any
			var labels []any
			var comments []any
			if len(assigneesJSON) > 0 {
				_ = json.Unmarshal(assigneesJSON, &assignees)
			}
			if len(labelsJSON) > 0 {
				_ = json.Unmarshal(labelsJSON, &labels)
			}
			if len(commentsJSON) > 0 {
				_ = json.Unmarshal(commentsJSON, &comments)
			}

			out = append(out, fiber.Map{
				"github_issue_id": gid,
				"number":          number,
				"state":           state,
				"title":           title,
				"description":     body,
				"author_login":    author,
				"assignees":       assignees,
				"labels":          labels,
				"comments_count":  commentsCount,
				"comments":        comments,
				"url":             url,
				"updated_at":      updated,
				"last_seen_at":    lastSeen,
			})
		}

		var total int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM github_issues WHERE project_id = $1
`, projectID).Scan(&total); err != nil {
			total = len(out)
		}

		return c.Status(fiber.StatusOK).JSON(PaginatedResponse("issues", out, p, total))
	}
}

func (h *ProjectDataHandler) PRs() fiber.Handler {
	return func(c *fiber.Ctx) error {
		projectID, err := h.projectIDForRead(c)
		if err != nil {
			return nil
		}

		p, err := ParsePagination(c, 20, 100)
		if err != nil {
			// response already written by ParsePagination on error
			return nil
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT github_pr_id, number, state, title, author_login, url, merged, 
       created_at_github, updated_at_github, closed_at_github, merged_at_github, last_seen_at
FROM github_pull_requests
WHERE project_id = $1
ORDER BY COALESCE(updated_at_github, last_seen_at) DESC
LIMIT $2 OFFSET $3
`, projectID, p.Limit, p.Offset)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "prs_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var gid int64
			var number int
			var state, title, author, url string
			var merged bool
			var createdAt, updated, closedAt, mergedAt *time.Time
			var lastSeen time.Time
			if err := rows.Scan(&gid, &number, &state, &title, &author, &url, &merged, &createdAt, &updated, &closedAt, &mergedAt, &lastSeen); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "prs_list_failed", "")
			}
			out = append(out, fiber.Map{
				"github_pr_id": gid,
				"number":       number,
				"state":        state,
				"title":        title,
				"author_login": author,
				"url":          url,
				"merged":       merged,
				"created_at":   createdAt,
				"updated_at":   updated,
				"closed_at":    closedAt,
				"merged_at":    mergedAt,
				"last_seen_at": lastSeen,
			})
		}

		var total int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM github_pull_requests WHERE project_id = $1
`, projectID).Scan(&total); err != nil {
			total = len(out)
		}

		return c.Status(fiber.StatusOK).JSON(PaginatedResponse("prs", out, p, total))
	}
}

func (h *ProjectDataHandler) Events() fiber.Handler {
	return func(c *fiber.Ctx) error {
		projectID, err := h.projectIDForRead(c)
		if err != nil {
			return nil
		}

		p, err := ParsePagination(c, 20, 100)
		if err != nil {
			// response already written by ParsePagination on error
			return nil
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT delivery_id, event, action, received_at
FROM github_events
WHERE project_id = $1
ORDER BY received_at DESC
LIMIT $2 OFFSET $3
`, projectID, p.Limit, p.Offset)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "events_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var deliveryID string
			var event string
			var action *string
			var receivedAt time.Time
			if err := rows.Scan(&deliveryID, &event, &action, &receivedAt); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "events_list_failed", "")
			}
			out = append(out, fiber.Map{
				"delivery_id": deliveryID,
				"event":       event,
				"action":      action,
				"received_at": receivedAt,
			})
		}

		var total int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM github_events WHERE project_id = $1
`, projectID).Scan(&total); err != nil {
			total = len(out)
		}

		return c.Status(fiber.StatusOK).JSON(PaginatedResponse("events", out, p, total))
	}
}

func (h *ProjectDataHandler) authorizeProject(c *fiber.Ctx) (uuid.UUID, bool, error) {
	if h.db == nil || h.db.Pool == nil {
		return uuid.Nil, false, respondProjectDataError(c, fiber.StatusServiceUnavailable, "db_not_configured")
	}
	sub, _ := c.Locals(auth.LocalUserID).(string)
	userID, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, false, respondProjectDataError(c, fiber.StatusUnauthorized, "invalid_user")
	}
	projectID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return uuid.Nil, false, respondProjectDataError(c, fiber.StatusBadRequest, "invalid_project_id")
	}

	var owner uuid.UUID
	err = h.db.Pool.QueryRow(c.Context(), `SELECT owner_user_id FROM projects WHERE id = $1`, projectID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, respondProjectDataError(c, fiber.StatusNotFound, "project_not_found")
	}
	if err != nil {
		return uuid.Nil, false, respondProjectDataError(c, fiber.StatusInternalServerError, "project_lookup_failed")
	}

	role, _ := c.Locals(auth.LocalRole).(string)
	ownerOK := owner == userID || role == "admin"
	return projectID, ownerOK, nil
}
