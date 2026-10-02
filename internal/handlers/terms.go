package handlers

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/terms"
)

// TermsHandler serves somebody their own Terms acceptance and records a new
// one. Both routes are about the caller only: the user comes from the token,
// never from the request, so nobody can accept on somebody else's behalf.
type TermsHandler struct{ db *db.DB }

func NewTermsHandler(d *db.DB) *TermsHandler { return &TermsHandler{db: d} }

// Get answers GET /me/terms.
func (h *TermsHandler) Get(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	s, err := terms.Get(c.Context(), h.db.Pool, uid)
	if err != nil {
		slog.Error("terms: read failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return c.JSON(s)
}

// Accept answers POST /me/terms/accept with {"version": "YYYY-MM-DD"}.
//
// The version is the one the page showed, not the server's current one.
// Recording "current" regardless would let a stale tab, still showing last
// month's text, record agreement to a text its reader never saw.
func (h *TermsHandler) Accept(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := c.BodyParser(&in); err != nil || in.Version == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "version_required"})
	}
	s, err := terms.Accept(c.Context(), h.db.Pool, uid, in.Version)
	if errors.Is(err, terms.ErrUnknownVersion) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":   "unknown_version",
			"message": "That version of the Terms was never published. Reload the page and try again.",
			"current": terms.Current(),
		})
	}
	if err != nil {
		slog.Error("terms: accept failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "accept_failed"})
	}
	return c.JSON(s)
}
