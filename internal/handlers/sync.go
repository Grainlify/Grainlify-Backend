package handlers

import (
	"github.com/jagadeesh/grainlify/backend/internal/httpx"

	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type SyncHandler struct {
	db *db.DB
}

func NewSyncHandler(d *db.DB) *SyncHandler {
	return &SyncHandler{db: d}
}

func (h *SyncHandler) EnqueueFullSync() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}

		var owner uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `SELECT owner_user_id FROM projects WHERE id = $1`, projectID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}

		role, _ := c.Locals(auth.LocalRole).(string)
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}

		_, _ = h.db.Pool.Exec(c.Context(), `
INSERT INTO sync_jobs (project_id, job_type, status, run_at)
VALUES ($1, 'sync_issues', 'pending', now()),
       ($1, 'sync_prs', 'pending', now())
`, projectID)

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"queued": true})
	}
}

// JobsForProject returns sync jobs for a project, newest first.
//
// Query parameters:
//   - limit: max results (default 50, max 200)
//   - offset: pagination offset (default 0)
//
// The response keeps the backward-compatible "jobs" array and adds
// limit/offset/total metadata consistent with the other paginated endpoints.
func (h *SyncHandler) JobsForProject() fiber.Handler {
	return func(c *fiber.Ctx) error {
		p, err := ParsePagination(c, 50, 200)
		if err != nil {
			// response already written by ParsePagination on error
			return nil
		}

		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_project_id", "")
		}

		var owner uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `SELECT owner_user_id FROM projects WHERE id = $1`, projectID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "project_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "project_lookup_failed", "")
		}

		role, _ := c.Locals(auth.LocalRole).(string)
		if owner != userID && role != "admin" {
			return httpx.RespondError(c, fiber.StatusForbidden, "forbidden", "")
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, job_type, status, run_at, attempts, last_error, created_at, updated_at
FROM sync_jobs
WHERE project_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3
`, projectID, p.Limit, p.Offset)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "jobs_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var id uuid.UUID
			var jobType, status string
			var runAt, createdAt, updatedAt time.Time
			var attempts int
			var lastErr *string
			if err := rows.Scan(&id, &jobType, &status, &runAt, &attempts, &lastErr, &createdAt, &updatedAt); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "jobs_list_failed", "")
			}
			out = append(out, fiber.Map{
				"id":         id.String(),
				"job_type":   jobType,
				"status":     status,
				"run_at":     runAt,
				"attempts":   attempts,
				"last_error": lastErr,
				"created_at": createdAt,
				"updated_at": updatedAt,
			})
		}

		var total int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1
`, projectID).Scan(&total); err != nil {
			total = len(out)
		}

		return c.Status(fiber.StatusOK).JSON(PaginatedResponse("jobs", out, p, total))
	}
}
