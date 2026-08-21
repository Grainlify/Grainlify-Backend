package handlers

import (
	"context"
	"net/mail"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The one reader of users.payout_contact_email.
//
// # Why a function rather than a query wherever it is needed
//
// The column exists to tell somebody about a payout and nothing else. A comment
// asking future code not to use it for anything else is a poor control - the
// traps file has an entry on exactly how poor - so the guard is structural: the
// column is read here, through a name that states the purpose, and
// TestPayoutContactColumnHasOneReader fails if a second reader appears anywhere
// in the package.
//
// Someone who wants to email a contributor for a different reason has to either
// use a function whose name contradicts them, or add a reader and watch a test
// go red. Neither is impossible, and both are deliberate, which is the point.
func payoutContactFor(ctx context.Context, d *db.DB, userID uuid.UUID) (string, error) {
	var addr *string
	if err := d.Pool.QueryRow(ctx,
		`SELECT payout_contact_email FROM users WHERE id = $1`, userID).Scan(&addr); err != nil {
		return "", err
	}
	if addr == nil {
		return "", nil
	}
	return *addr, nil
}

// PayoutContactHandler serves the optional contact address on the payout screen.
type PayoutContactHandler struct{ db *db.DB }

func NewPayoutContactHandler(d *db.DB) *PayoutContactHandler {
	return &PayoutContactHandler{db: d}
}

type payoutContactRequest struct {
	// Empty string clears it. A separate DELETE route would be tidier REST and
	// worse here: removal has to be as reachable as saving, and one endpoint
	// that both sets and clears cannot end up with the clear half unbuilt.
	Email string `json:"email"`
}

// Get returns the stored contact address, or "" when there is none.
func (h *PayoutContactHandler) Get(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	addr, err := payoutContactFor(c.Context(), h.db, uid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "contact_lookup_failed"})
	}
	return c.JSON(fiber.Map{"email": addr})
}

// Put stores or clears it.
func (h *PayoutContactHandler) Put(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	var req payoutContactRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_json"})
	}
	addr := strings.TrimSpace(req.Email)

	if addr == "" {
		// Removal, and it must not be able to fail for a reason saving cannot.
		// No validation, no confirmation: somebody withdrawing their address is
		// entitled to have that work on the first attempt.
		if _, err := h.db.Pool.Exec(c.Context(),
			`UPDATE users SET payout_contact_email = NULL, updated_at = now() WHERE id = $1`, uid); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "contact_clear_failed"})
		}
		return c.JSON(fiber.Map{"email": ""})
	}

	// Validated only for shape. An address we cannot parse is one we could not
	// send to, and telling somebody now beats a bounce they never learn about -
	// but deliverability is not checked, because a probe would be a message
	// they did not ask for.
	if _, err := mail.ParseAddress(addr); err != nil || !strings.Contains(addr, ".") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":  "email_malformed",
			"detail": "That does not look like an email address we could send to.",
		})
	}
	if len(addr) > 320 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "email_too_long"})
	}

	if _, err := h.db.Pool.Exec(c.Context(),
		`UPDATE users SET payout_contact_email = $1, updated_at = now() WHERE id = $2`, addr, uid); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "contact_save_failed"})
	}
	return c.JSON(fiber.Map{"email": addr})
}
