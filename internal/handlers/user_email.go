package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/useremail"
)

// UserEmailHandler serves somebody their own stored email address and the two
// controls over it: the master switch, and removal.
//
// Every route here is about the caller's own address. There is deliberately no
// route that reads anybody else's - see TestStoredEmailHasOneReaderPerPurpose,
// which fails if a second place in the codebase reads the column.
type UserEmailHandler struct{ db *db.DB }

func NewUserEmailHandler(d *db.DB) *UserEmailHandler { return &UserEmailHandler{db: d} }

func (h *UserEmailHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	raw, ok := c.Locals(auth.LocalUserID).(string)
	if !ok || raw == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// Get answers GET /me/email.
func (h *UserEmailHandler) Get(c *fiber.Ctx) error {
	uid, ok := h.userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	s, err := useremail.Get(c.Context(), h.db.Pool, uid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return c.JSON(s)
}

// Put answers PUT /me/email, which sets the master switch only.
//
// It cannot set an address. The address comes from GitHub at sign-in and
// nowhere else, so there is no endpoint that accepts one: an address somebody
// typed here would be unverified, and an unverified address is a way to have
// Grainlify email a stranger.
func (h *UserEmailHandler) Put(c *fiber.Ctx) error {
	uid, ok := h.userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		Enabled *bool `json:"enabled"`
		// Allow undoes a previous removal, so the next sign-in stores the
		// address again. Only the person themselves can do this.
		Allow *bool `json:"allow"`
	}
	if err := c.BodyParser(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "malformed"})
	}
	if in.Enabled == nil && in.Allow == nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "nothing_to_change"})
	}
	if in.Enabled != nil {
		if err := useremail.SetEnabled(c.Context(), h.db.Pool, uid, *in.Enabled); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "update_failed"})
		}
	}
	if in.Allow != nil && *in.Allow {
		if err := useremail.Allow(c.Context(), h.db.Pool, uid); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "update_failed"})
		}
	}
	s, err := useremail.Get(c.Context(), h.db.Pool, uid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return c.JSON(s)
}

// Delete answers DELETE /me/email: removes the stored address and records the
// refusal, so signing in again does not put it back.
func (h *UserEmailHandler) Delete(c *fiber.Ctx) error {
	uid, ok := h.userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	if err := useremail.Remove(c.Context(), h.db.Pool, uid); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "delete_failed"})
	}
	s, err := useremail.Get(c.Context(), h.db.Pool, uid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	return c.JSON(s)
}
