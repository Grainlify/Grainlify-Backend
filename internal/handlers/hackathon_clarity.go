package handlers

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// HackathonClarityHandler serves AI-specs.md §7's issue-clarity rating.
//
// Nothing here is on the path to submitting a PR. The rating is offered after
// the fact and can be ignored forever: a required rating standing between a
// contributor and their work produces compliance clicks, not signal.
type HackathonClarityHandler struct {
	db *db.DB
}

func NewHackathonClarityHandler(d *db.DB) *HackathonClarityHandler {
	return &HackathonClarityHandler{db: d}
}

type clarityRatingRequest struct {
	Rating  int    `json:"rating"`
	Comment string `json:"comment"`
}

// Submit handles POST /hackathon-assignments/:id/clarity-rating.
func (h *HackathonClarityHandler) Submit() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		assignmentID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_assignment_id"})
		}
		var req clarityRatingRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		err = hackathon.SubmitClarityRating(c.Context(), h.db.Pool, assignmentID, userID, req.Rating, req.Comment)
		switch {
		case err == nil:
			return c.JSON(fiber.Map{"ok": true})
		case errors.Is(err, hackathon.ErrClarityNotYourAssignment):
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_your_assignment"})
		case errors.Is(err, hackathon.ErrClarityWindowClosed):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":   "rating_window_closed",
				"message": "Ratings close when results are published, so they always predate anyone knowing their payout.",
			})
		default:
			slog.Warn("clarity rating", "error", err)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "rating_failed", "message": err.Error()})
		}
	}
}

// ForMaintainer handles GET /projects/:id/grainhack/clarity?hackathon_id=...
//
// Aggregates only, and only once the event has closed. Never individual
// ratings, never attributed comments.
func (h *HackathonClarityHandler) ForMaintainer() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		hackathonID, err := uuid.Parse(c.Query("hackathon_id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		// Owner-or-admin, matching how the rest of the maintainer-facing
		// GrainHack surfaces are gated.
		var owner uuid.UUID
		if err := h.db.Pool.QueryRow(c.Context(),
			`SELECT owner_user_id FROM projects WHERE id = $1`, projectID).Scan(&owner); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "project_not_found"})
		}
		role, _ := c.Locals(auth.LocalRole).(string)
		if owner != userID && role != "admin" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
		}

		agg, err := hackathon.ClarityForMaintainer(c.Context(), h.db.Pool, hackathonID, projectID)
		if err != nil {
			slog.Error("clarity aggregate", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "clarity_fetch_failed"})
		}
		return c.JSON(agg)
	}
}
