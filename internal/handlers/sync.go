package handlers

import (
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
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

type SyncHandler struct {
	db *db.DB
}

func NewSyncHandler(d *db.DB) *SyncHandler {
	return &SyncHandler{db: d}
}

// EnqueueFullSync enqueues a full project sync.
//
// Idempotency / Deduplication Strategy:
// 1. If the client provides an "Idempotency-Key" header:
//    - The request is validated to ensure the key is <= 255 characters.
//    - We check the idempotency_keys table for a cached response scoped to the user.
//    - Cached successful responses are valid for 24 hours.
// 2. If no "Idempotency-Key" header is provided:
//    - We fall back to a natural key generated from: "sync:<project_id>:manual:<time_window_slot>".
//    - The time window slot changes every 5 minutes (300 seconds) to prevent spamming.
//    - The natural key is scoped to the user in idempotency_keys and cached for 5 minutes.
// 3. Concurrency Protection:
//    - We insert the idempotency key BEFORE executing the underlying sync jobs.
//    - The insert uses ON CONFLICT (user_id, idempotency_key) DO NOTHING.
//    - If RowsAffected() == 0, another concurrent request already acquired the key. We return a cached/success response.
//    - If RowsAffected() == 1, we execute the INSERT into sync_jobs. If that fails, we delete the key.
func (h *SyncHandler) EnqueueFullSync() fiber.Handler {
	return func(c *fiber.Ctx) error {
		idempotencyKey := strings.TrimSpace(c.Get("Idempotency-Key"))
		if idempotencyKey != "" && len(idempotencyKey) > 255 {
			return httpx.RespondError(c, fiber.StatusBadRequest, "idempotency_key_too_long", "Idempotency-Key header must be 255 characters or less")
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

		// Determine strategy: client-provided key vs. natural key (5-minute window)
		isNaturalKey := false
		if idempotencyKey == "" {
			isNaturalKey = true
			// natural key time slot: 5-minute interval
			timeWindow := time.Now().Unix() / 300
			idempotencyKey = fmt.Sprintf("sync:%s:manual:%d", projectID, timeWindow)
		}

		// Check for existing cached response
		var cachedStatus int
		var cachedBody string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT response_status, response_body
FROM idempotency_keys
WHERE user_id = $1 AND idempotency_key = $2 AND expires_at > now()
LIMIT 1
`, userID, idempotencyKey).Scan(&cachedStatus, &cachedBody)

		if err == nil {
			slog.Info("sync handler idempotency cache hit",
				"user_id", userID.String(),
				"idempotency_key_hash", fmt.Sprintf("%x", hashString(idempotencyKey)[:8]),
				"project_id", projectID.String(),
			)
			var cachedResponse fiber.Map
			if jsonErr := json.Unmarshal([]byte(cachedBody), &cachedResponse); jsonErr == nil {
				return c.Status(cachedStatus).JSON(cachedResponse)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("sync handler idempotency key lookup failed",
				"user_id", userID.String(),
				"error", err,
			)
		}

		// Determine expiration and success response representation
		var expiresAt time.Time
		if isNaturalKey {
			expiresAt = time.Now().Add(5 * time.Minute)
		} else {
			expiresAt = time.Now().Add(24 * time.Hour)
		}

		successResponse := fiber.Map{"queued": true}
		successResponseJSON, _ := json.Marshal(successResponse)

		// Race prevention: insert the idempotency key first.
		tag, err := h.db.Pool.Exec(c.Context(), `
INSERT INTO idempotency_keys (user_id, idempotency_key, response_status, response_body, created_at, expires_at)
VALUES ($1, $2, $3, $4, now(), $5)
ON CONFLICT (user_id, idempotency_key) DO NOTHING
`, userID, idempotencyKey, fiber.StatusAccepted, string(successResponseJSON), expiresAt)

		if err != nil {
			slog.Warn("failed to insert idempotency key, proceeding to execute sync without cache",
				"user_id", userID.String(),
				"error", err,
			)
			_, err = h.db.Pool.Exec(c.Context(), `
INSERT INTO sync_jobs (project_id, job_type, status, run_at)
VALUES ($1, 'sync_issues', 'pending', now()),
       ($1, 'sync_prs', 'pending', now())
`, projectID)
			if err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "sync_enqueue_failed", "")
			}
			return c.Status(fiber.StatusAccepted).JSON(successResponse)
		}

		if tag.RowsAffected() == 0 {
			// Concurrency win: another request inserted this key first.
			slog.Info("sync handler idempotency concurrent request deduped",
				"user_id", userID.String(),
				"idempotency_key_hash", fmt.Sprintf("%x", hashString(idempotencyKey)[:8]),
				"project_id", projectID.String(),
			)
			return c.Status(fiber.StatusAccepted).JSON(successResponse)
		}

		// Execute the underlying sync jobs insert
		_, err = h.db.Pool.Exec(c.Context(), `
INSERT INTO sync_jobs (project_id, job_type, status, run_at)
VALUES ($1, 'sync_issues', 'pending', now()),
       ($1, 'sync_prs', 'pending', now())
`, projectID)
		if err != nil {
			slog.Error("failed to write sync jobs to db, rolling back idempotency key",
				"user_id", userID.String(),
				"project_id", projectID.String(),
				"error", err,
			)
			// Delete key so retry can re-attempt
			_, _ = h.db.Pool.Exec(c.Context(), `
DELETE FROM idempotency_keys WHERE user_id = $1 AND idempotency_key = $2
`, userID, idempotencyKey)
			return httpx.RespondError(c, fiber.StatusInternalServerError, "sync_enqueue_failed", "")
		}

		return c.Status(fiber.StatusAccepted).JSON(successResponse)
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
