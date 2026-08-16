package handlers

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
)

// A contributor's own support history.
//
// It exists so somebody can see what they already sent instead of asking in
// Telegram, which is currently the only way to find out. Reports are returned
// newest first and scoped to the caller: the user id comes from the verified
// JWT, never from a parameter, so there is no id to tamper with.
//
// # What "status" means here, honestly
//
// support_requests has no status column. Nothing records that a report was
// read, answered or closed, because no such state is captured anywhere - the
// reply happens in Telegram or Discord and never comes back to the row.
//
// So `status` is currently derived from delivery and always reads "received".
// It is a real field rather than a hardcoded string in the UI because a real
// status is a planned follow-up, and the shape should not change when it
// arrives. What is deliberately NOT done is inventing "open" or "in progress"
// from a timestamp: that would tell a contributor their report is being worked
// on when nothing in the system knows whether it is.
//
// deliveredToTeam is separate and true. It is the difference between "we have
// your report" and "somebody was told about it", and those come apart when a
// sink fails - the row is written first and delivery is best effort, precisely
// so a Telegram outage never costs somebody their report.

type mySupportRequest struct {
	ID       uuid.UUID `json:"id"`
	Category string    `json:"category"`
	Message  string    `json:"message"`
	PageURL  string    `json:"page_url,omitempty"`
	// Status is "received" today. See the note above before adding values.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// DeliveredToTeam is false when every sink failed. The report is safe on
	// the row either way, but nobody was notified, and the contributor is
	// better off knowing that than wondering why there was no reply.
	DeliveredToTeam bool `json:"delivered_to_team"`
	HasScreenshot   bool `json:"has_screenshot"`
}

// Mine returns the caller's own support requests.
func (h *SupportRequestsHandler) Mine() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "database_unavailable"})
		}
		// Read the way every other authenticated handler does: the middleware
		// stores the subject as a string under auth.LocalUserID. Asserting a
		// uuid.UUID here fails silently for every caller and returns 401.
		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, category, message, COALESCE(page_url, ''), created_at,
       (discord_delivered_at IS NOT NULL
         OR telegram_delivered_at IS NOT NULL
         OR telegram_admin_dm_delivered_at IS NOT NULL) AS delivered,
       screenshot_url IS NOT NULL AS has_screenshot
FROM support_requests
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT 50
`, userID)
		if err != nil {
			slog.Error("support requests: could not list own reports",
				"user_id", userID, "error", err, "request_id", c.Locals("requestid"))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "could_not_list_reports"})
		}
		defer rows.Close()

		// Never nil: an empty list must serialise as [] rather than null, or the
		// page renders "cannot read length of null" instead of an empty state.
		out := []mySupportRequest{}
		for rows.Next() {
			var r mySupportRequest
			if err := rows.Scan(&r.ID, &r.Category, &r.Message, &r.PageURL,
				&r.CreatedAt, &r.DeliveredToTeam, &r.HasScreenshot); err != nil {
				slog.Error("support requests: scan failed", "user_id", userID, "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "could_not_list_reports"})
			}
			r.Status = "received"
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			slog.Error("support requests: iteration failed", "user_id", userID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "could_not_list_reports"})
		}

		return c.JSON(fiber.Map{"support_requests": out})
	}
}
