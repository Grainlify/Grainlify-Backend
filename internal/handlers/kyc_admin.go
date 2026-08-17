package handlers

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Admin reset of a contributor's KYC status.
//
// A refused verification is terminal in the product: canStartNewKYCSession
// allows only "", "expired" and "not_started", so a contributor whose documents
// were rejected cannot start again from the UI at all. Until this existed the
// only exit was an admin running UPDATE by hand against production - which
// happened, for four contributors, and left no record of who did it or why.
//
// What it deliberately does NOT do:
//
//   - It does not clear kyc_data. That field holds Didit's decision, including
//     the reason for the refusal, and it is the only copy we have on our side.
//     Nulling it is not required for a retry - canStartNewKYCSession reads the
//     status, and Start() reads the session id - so destroying it would be an
//     unnecessary, irreversible loss of the record a dispute turns on. It is
//     overwritten naturally by the next decision.
//   - It does not mark anybody verified. The only status it can produce is
//     "expired", which means "try again", not "you passed". A reset must never
//     be a route to granting verification without verifying.

type KYCAdminHandler struct {
	db     *db.DB
	notify *notifications.Service
}

func NewKYCAdminHandler(d *db.DB, notify *notifications.Service) *KYCAdminHandler {
	return &KYCAdminHandler{db: d, notify: notify}
}

type kycResetRequest struct {
	Reason string `json:"reason"`
}

// Reset handles POST /admin/kyc/:id/reset.
//
// Returns the previous status so the caller can see what it undid, which is
// also what makes an accidental reset obvious immediately rather than at
// settlement.
func (h *KYCAdminHandler) Reset() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		actorStr, _ := c.Locals(auth.LocalUserID).(string)
		actorID, err := uuid.Parse(actorStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		subjectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_user_id"})
		}

		var req kycResetRequest
		_ = c.BodyParser(&req)
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			// Required because the audit row is the whole point. A reset with
			// no stated reason records that it happened but not why, which is
			// the half that matters when somebody asks later.
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "reason_required",
				"message": "A reason is recorded against this reset. Say why the contributor is being allowed to verify again.",
			})
		}

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reset_failed"})
		}
		defer func() { _ = tx.Rollback(c.Context()) }()

		// Read the prior state inside the transaction and lock the row, so a
		// concurrent webhook cannot land a decision between the read and the
		// write and have it silently discarded.
		var prevStatus, prevSessionID *string
		err = tx.QueryRow(c.Context(), `
SELECT kyc_status, kyc_session_id
FROM users
WHERE id = $1
FOR UPDATE
`, subjectID).Scan(&prevStatus, &prevSessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
		}
		if err != nil {
			slog.Error("kyc reset: read failed", "subject", subjectID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reset_failed"})
		}

		// Refuse to reset somebody who is already verified. Doing so would
		// take away a verification they hold, which is the opposite of what
		// this endpoint is for and is not something a misclick should manage.
		if prevStatus != nil && *prevStatus == "verified" {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":   "already_verified",
				"message": "This contributor is verified. Resetting would remove that; it is not what this action is for.",
			})
		}

		if _, err := tx.Exec(c.Context(), `
UPDATE users
SET kyc_status = 'expired',
    kyc_session_id = NULL,
    updated_at = now()
WHERE id = $1
`, subjectID); err != nil {
			slog.Error("kyc reset: update failed", "subject", subjectID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reset_failed"})
		}

		if _, err := tx.Exec(c.Context(), `
INSERT INTO kyc_reset_audit
  (subject_user_id, previous_status, previous_session_id, actor_user_id, reason)
VALUES ($1, $2, $3, $4, $5)
`, subjectID, prevStatus, prevSessionID, actorID, reason); err != nil {
			slog.Error("kyc reset: audit insert failed", "subject", subjectID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reset_failed"})
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reset_failed"})
		}

		prev := ""
		if prevStatus != nil {
			prev = *prevStatus
		}
		slog.Info("kyc status reset by admin",
			"subject_user_id", subjectID, "actor_user_id", actorID,
			"previous_status", prev, "reason", reason)

		// Best-effort: a failed notification must not undo a recorded reset.
		// Worth sending, though - a refused verification is terminal in the UI,
		// so somebody who was stuck has no way to discover the door reopened.
		if h.notify != nil {
			h.notify.Notify(c.Context(), subjectID, notifications.TypeKYCReset,
				"You can verify your identity again",
				"An admin has reset your verification so you can try again: "+reason,
				notifications.SettingsLink(notifications.SubtabPayout))
		}

		return c.JSON(fiber.Map{"ok": true, "previous_status": prev, "status": "expired"})
	}
}

// History handles GET /admin/kyc/:id/resets - every reset ever applied to one
// contributor, newest first. The audit is only useful if it is readable
// without database access, which was the state that made the manual UPDATE
// invisible in the first place.
func (h *KYCAdminHandler) History() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		subjectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_user_id"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT r.previous_status, r.previous_session_id, r.reason, r.created_at::text,
       COALESCE(ga.login, '')
FROM kyc_reset_audit r
LEFT JOIN github_accounts ga ON ga.user_id = r.actor_user_id
WHERE r.subject_user_id = $1
ORDER BY r.created_at DESC
`, subjectID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "history_failed"})
		}
		defer rows.Close()

		out := []fiber.Map{}
		for rows.Next() {
			var prevStatus, prevSession, reason *string
			var createdAt, actorLogin string
			if err := rows.Scan(&prevStatus, &prevSession, &reason, &createdAt, &actorLogin); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "history_failed"})
			}
			out = append(out, fiber.Map{
				"previous_status":     prevStatus,
				"previous_session_id": prevSession,
				"reason":              reason,
				"created_at":          createdAt,
				"actor_github_login":  actorLogin,
			})
		}
		return c.JSON(fiber.Map{"resets": out})
	}
}
