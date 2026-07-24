package handlers

import (
	"github.com/jagadeesh/grainlify/backend/internal/httpx"

	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

const (
	maxTitleLen       = 200
	maxDescriptionLen = 2000
	maxLocationLen    = 500
)

type OpenSourceWeekHandler struct {
	db *db.DB
}

func NewOpenSourceWeekHandler(d *db.DB) *OpenSourceWeekHandler {
	return &OpenSourceWeekHandler{db: d}
}

// ListPublic returns events that are not draft (upcoming/running/completed).
func (h *OpenSourceWeekHandler) ListPublic() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, title, description, location, status, start_at, end_at, created_at, updated_at
FROM open_source_week_events
WHERE status <> 'draft'
ORDER BY start_at DESC
LIMIT 100
`)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_events_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var id uuid.UUID
			var title, status string
			var desc, location *string
			var startAt, endAt, createdAt, updatedAt time.Time
			if err := rows.Scan(&id, &title, &desc, &location, &status, &startAt, &endAt, &createdAt, &updatedAt); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_events_list_failed", "")
			}
			out = append(out, fiber.Map{
				"id":          id.String(),
				"title":       title,
				"description": desc,
				"location":    location,
				"status":      status,
				"start_at":    startAt,
				"end_at":      endAt,
				"created_at":  createdAt,
				"updated_at":  updatedAt,
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"events": out})
	}
}

func (h *OpenSourceWeekHandler) GetPublic() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		evID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_event_id", "")
		}

		var title, status string
		var desc, location *string
		var startAt, endAt, createdAt, updatedAt time.Time
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT title, description, location, status, start_at, end_at, created_at, updated_at
FROM open_source_week_events
WHERE id = $1 AND status <> 'draft'
`, evID).Scan(&title, &desc, &location, &status, &startAt, &endAt, &createdAt, &updatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.RespondError(c, fiber.StatusNotFound, "event_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_event_get_failed", "")
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"event": fiber.Map{
				"id":          evID.String(),
				"title":       title,
				"description": desc,
				"location":    location,
				"status":      status,
				"start_at":    startAt,
				"end_at":      endAt,
				"created_at":  createdAt,
				"updated_at":  updatedAt,
			},
		})
	}
}

type OpenSourceWeekAdminHandler struct {
	db *db.DB
}

func NewOpenSourceWeekAdminHandler(d *db.DB) *OpenSourceWeekAdminHandler {
	return &OpenSourceWeekAdminHandler{db: d}
}

func (h *OpenSourceWeekAdminHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, title, description, location, status, start_at, end_at, created_at, updated_at
FROM open_source_week_events
ORDER BY start_at DESC
LIMIT 200
`)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_events_list_failed", "")
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var id uuid.UUID
			var title, status string
			var desc, location *string
			var startAt, endAt, createdAt, updatedAt time.Time
			if err := rows.Scan(&id, &title, &desc, &location, &status, &startAt, &endAt, &createdAt, &updatedAt); err != nil {
				return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_events_list_failed", "")
			}
			out = append(out, fiber.Map{
				"id":          id.String(),
				"title":       title,
				"description": desc,
				"location":    location,
				"status":      status,
				"start_at":    startAt,
				"end_at":      endAt,
				"created_at":  createdAt,
				"updated_at":  updatedAt,
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"events": out})
	}
}

type oswCreateRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Status      string `json:"status"`   // upcoming|running|completed|draft
	StartAt     string `json:"start_at"` // RFC3339
	EndAt       string `json:"end_at"`   // RFC3339
}

// validateCreate validates an OSW create request and returns an error code on failure.
// An empty string means validation passed.
func validateCreate(req oswCreateRequest) string {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return "title_required"
	}
	if len(title) > maxTitleLen {
		return "title_too_long"
	}
	if len(strings.TrimSpace(req.Description)) > maxDescriptionLen {
		return "description_too_long"
	}
	if len(strings.TrimSpace(req.Location)) > maxLocationLen {
		return "location_too_long"
	}
	status := strings.TrimSpace(req.Status)
	if status != "upcoming" && status != "running" && status != "completed" && status != "draft" {
		return "invalid_status"
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(req.StartAt)); err != nil {
		return "invalid_start_at"
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(req.EndAt)); err != nil {
		return "invalid_end_at"
	}
	startAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(req.StartAt))
	endAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(req.EndAt))

	// All time comparisons explicitly happen in a single consistent timezone (UTC).
	startAt = startAt.UTC()
	endAt = endAt.UTC()

	if !endAt.After(startAt) {
		return "end_at_must_be_after_start_at"
	}
	return ""
}

// IsTimeInCampaignWindow determines whether the given 'now' time falls within the campaign window defined by startAt and endAt.
// All time comparisons are strictly done in UTC.
// The campaign window is inclusive of the start time and exclusive of the end time: [startAt, endAt).
// Thus:
// - exactly at startAt: active (true)
// - one second before startAt: inactive (false)
// - exactly at endAt: inactive (false)
// - one second after endAt: inactive (false)
// - inside the window: active (true)
func IsTimeInCampaignWindow(now, startAt, endAt time.Time) bool {
	nowUTC := now.UTC()
	startUTC := startAt.UTC()
	endUTC := endAt.UTC()

	return (nowUTC.Equal(startUTC) || nowUTC.After(startUTC)) && nowUTC.Before(endUTC)
}

func (h *OpenSourceWeekAdminHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		var req oswCreateRequest
		if err := c.BodyParser(&req); err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "")
		}

		if errCode := validateCreate(req); errCode != "" {
			return httpx.RespondError(c, fiber.StatusBadRequest, httpx.Code(errCode), "")
		}

		title := strings.TrimSpace(req.Title)
		status := strings.TrimSpace(req.Status)
		if status == "" {
			status = "upcoming"
		}
		startAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(req.StartAt))
		endAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(req.EndAt))

		// Explicitly convert to UTC before database persistence to align with our timezone convention.
		startAt = startAt.UTC()
		endAt = endAt.UTC()

		var id uuid.UUID
		err := h.db.Pool.QueryRow(c.Context(), `
INSERT INTO open_source_week_events (title, description, location, status, start_at, end_at)
VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6)
RETURNING id
`, title, strings.TrimSpace(req.Description), strings.TrimSpace(req.Location), status, startAt, endAt).Scan(&id)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_event_create_failed", "")
		}

		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id.String()})
	}
}

func (h *OpenSourceWeekAdminHandler) Delete() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		evID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_event_id", "")
		}
		ct, err := h.db.Pool.Exec(c.Context(), `DELETE FROM open_source_week_events WHERE id = $1`, evID)
		if errors.Is(err, pgx.ErrNoRows) || ct.RowsAffected() == 0 {
			return httpx.RespondError(c, fiber.StatusNotFound, "event_not_found", "")
		}
		if err != nil {
			return httpx.RespondError(c, fiber.StatusInternalServerError, "osw_event_delete_failed", "")
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
