package handlers

import (
	"encoding/json"
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
// This existed because the only alternative was an admin running UPDATE by
// hand against production - which happened, for four contributors, and left no
// record of who did it or why.
//
// Its scope narrowed once canStartNewKYCSession began accepting "rejected": a
// refused contributor can now retry without anybody's help, so this is no
// longer the exit from that dead end. What it still covers is every state a
// contributor cannot leave on their own - above all in_review, where a session
// is waiting on a decision that may never come, and where re-attempting would
// produce a duplicate session rather than resolve the first.
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
	// ReasonCode is one of kycResetReasons. It decides what the contributor is
	// told, so it is required - unlike the social-follow queue, where a bare
	// note is still accepted for backwards compatibility, this endpoint has
	// never been called by any client and has no cached bundle to break.
	ReasonCode string `json:"reason_code"`
	// Note is the admin's own words, appended to the message the contributor
	// receives. Optional except for the 'other' code, which carries no message
	// of its own.
	Note string `json:"note"`
	// Reason is the internal justification, recorded and never sent. Kept
	// separate from Note because an audit entry and a message to the person it
	// concerns are different documents - previously they were one string, and
	// the admin's reasoning went out verbatim to the contributor.
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
		note := strings.TrimSpace(req.Note)

		chosen, ok := kycReasonByCode(strings.TrimSpace(req.ReasonCode))
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "invalid_reason_code",
				"message": "Pick one of the listed reasons. It decides what the contributor is told.",
			})
		}
		if chosen.NeedsNote && note == "" {
			// 'other' names no problem on its own. Sending it with no note
			// would deliver a refusal with no content - the dead end this
			// exists to remove, rebuilt one layer up.
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "note_required",
				"message": "This reason needs a note: it is the only thing the contributor will have to act on.",
			})
		}
		if reason == "" {
			// Still required, and still separate. The audit row is the whole
			// point: a reset that records what the contributor was told but
			// not why we told them is half a record.
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
		// kyc_data is read here and stored on the audit row because this is
		// the last moment it exists. Both the webhook and the status poll
		// overwrite the column wholesale on the next decision, and Start()
		// overwrites it when the contributor retries - which, now that a
		// refused contributor can retry unaided, may be minutes from now.
		// Whatever this reset was a response to is unrecoverable afterwards.
		var prevKYCData []byte
		err = tx.QueryRow(c.Context(), `
SELECT kyc_status, kyc_session_id, kyc_data
FROM users
WHERE id = $1
FOR UPDATE
`, subjectID).Scan(&prevStatus, &prevSessionID, &prevKYCData)
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

		var auditID uuid.UUID
		if err := tx.QueryRow(c.Context(), `
INSERT INTO kyc_reset_audit
  (subject_user_id, previous_status, previous_session_id, actor_user_id,
   reason, reason_code, note, previous_kyc_data)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
RETURNING id
`, subjectID, prevStatus, prevSessionID, actorID,
			reason, chosen.Code, note, prevKYCData).Scan(&auditID); err != nil {
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
		// But it must not be invisible either - three resets were applied by
		// hand against production and told nobody, and the only reason that is
		// known is that the operator wrote it into the reason text. The
		// outcome is recorded either way.
		//
		// In-app only. See notifications.NotifyInApp: no account has a stored
		// email address, and persisting one is being decided separately.
		//
		// The link goes to the BILLING subtab, not payout. Verification lives
		// in BillingTab; the payout screen's own copy tells you to go to
		// Billing. Same bug as the maintainer notification that pointed at the
		// contributor view - a link to a page where the action is not.
		notified := false
		notifyErr := ""
		if h.notify != nil {
			res := h.notify.NotifyInApp(c.Context(), subjectID, notifications.TypeKYCReset,
				"You can verify your identity again",
				kycResetMessage(chosen, note),
				notifications.SettingsLink(notifications.SubtabBilling))
			notified = res.Created
			switch {
			case res.Err != nil:
				notifyErr = truncateRunes(res.Err.Error(), 500)
			case res.Suppressed:
				notifyErr = "suppressed by the contributor's notification preferences"
			}
		} else {
			notifyErr = "notification service not configured"
		}

		if _, err := h.db.Pool.Exec(c.Context(), `
UPDATE kyc_reset_audit
SET notified_at = CASE WHEN $2 THEN now() ELSE NULL END,
    notify_error = NULLIF($3, '')
WHERE id = $1
`, auditID, notified, notifyErr); err != nil {
			// The reset itself is committed and correct; only the record of
			// whether we told them failed to save. Loud, because this column
			// exists precisely so nobody has to guess.
			slog.Error("kyc reset: could not record notification outcome",
				"audit_id", auditID, "subject", subjectID, "notified", notified, "error", err)
		}

		return c.JSON(fiber.Map{
			"ok":              true,
			"previous_status": prev,
			"status":          "expired",
			"reason_code":     chosen.Code,
			// Returned so the admin screen can show what the contributor was
			// actually told, rather than reconstructing it and drifting.
			"message_sent": kycResetMessage(chosen, note),
			"notified":     notified,
		})
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

		// previous_kyc_data is deliberately not selected. It is stored so the
		// evidence behind a decision survives, not so it can be served: it is
		// the raw provider decision, and this endpoint feeds a screen. Anyone
		// who needs it is answering a dispute and can query for it.
		rows, err := h.db.Pool.Query(c.Context(), `
SELECT r.previous_status, r.previous_session_id, r.reason, r.created_at::text,
       COALESCE(ga.login, ''),
       r.reason_code, r.note,
       (r.notified_at IS NOT NULL), r.notify_error
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
			var prevStatus, prevSession, reason, reasonCode, note, notifyError *string
			var createdAt, actorLogin string
			var notified bool
			if err := rows.Scan(&prevStatus, &prevSession, &reason, &createdAt, &actorLogin,
				&reasonCode, &note, &notified, &notifyError); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "history_failed"})
			}
			row := fiber.Map{
				"previous_status":     prevStatus,
				"previous_session_id": prevSession,
				"reason":              reason,
				"created_at":          createdAt,
				"actor_github_login":  actorLogin,
				"reason_code":         reasonCode,
				"note":                note,
				// The question the audit could not answer before: was the
				// contributor actually told? Three resets reached nobody and
				// nothing recorded it.
				"notified":     notified,
				"notify_error": notifyError,
			}
			// The label and the message as they stand today, resolved from the
			// code rather than stored per row - so a wording improvement
			// applies to the history too, and an unrecognised code (a reason
			// retired since) degrades to the bare code rather than vanishing.
			if reasonCode != nil {
				if r, ok := kycReasonByCode(*reasonCode); ok {
					row["reason_label"] = r.Label
					row["message_sent"] = kycResetMessage(r, derefOrEmpty(note))
				}
			}
			out = append(out, row)
		}
		return c.JSON(fiber.Map{"resets": out})
	}
}

// ReasonCodes handles GET /admin/kyc/reason-codes.
//
// The closed list, served rather than duplicated in the frontend. Two copies
// of one list is how the same rule comes to disagree with itself; this way the
// review screen renders whatever the send path will accept, by construction.
//
// Message is included so the admin can see exactly what the contributor will
// read before choosing. A reason picker that hides the resulting message asks
// somebody to choose blind.
func (h *KYCAdminHandler) ReasonCodes() fiber.Handler {
	return func(c *fiber.Ctx) error {
		out := make([]fiber.Map, 0, len(kycResetReasons))
		for _, r := range kycResetReasons {
			out = append(out, fiber.Map{
				"code":       r.Code,
				"label":      r.Label,
				"message":    r.Message,
				"needs_note": r.NeedsNote,
			})
		}
		return c.JSON(fiber.Map{"reason_codes": out})
	}
}

// Pending handles GET /admin/kyc/pending - the review queue.
//
// Everything waiting on a decision: in_review (Didit routes these to OUR
// queue, not theirs) and rejected. Both are here because they are two halves
// of one job - the second is somebody a decision has already been made about
// who may still need telling why.
//
// THE TEST FOR ADDING A FIELD HERE: does it carry a document, a decision, or
// provider text? If it does, it does not belong. If it does not, it can.
//
// The rule has held. Nothing personal from the provider reaches this endpoint.
//
// Two identifiers do, and both pass the test because they name a session
// without describing a person:
//
//	kyc_session_id  the provider's UUID for the session
//	session_number  the provider's short counter for it (3, 12, 41...), which
//	                is what their console's verification table displays
//
// Worth recording how close this came to being widened, because the argument
// was good. Matching a queue row to a submission in the Didit console looked
// like it required the legal name: the console lists people by name, and has
// no documented search by session id and no URL that opens a single session.
// The case for showing the name was that the reviewer already sees it there
// anyway, so nothing would reach anybody new.
//
// It was not needed. The console displays session_number, which we had been
// storing all along and never used - so the match key was already in the data,
// and no personal detail had to be exposed to find it.
//
// The general form, for the next time this comes up: when matching seems to
// require a personal attribute, look for an identifier first. There is usually
// one, and it is usually already stored. An exception that gets made once is an
// invitation to make it again.
//
// What it deliberately does NOT return: kyc_data, any document image, or any
// provider warning text. The admin reads the provider console for the detail;
// this endpoint carries only what is needed to identify the person, see how
// long they have waited, and choose a reason. `suggested_reason_codes` is
// derived from the stored decision on the way out and is a suggestion only -
// the send path never consults it.
func (h *KYCAdminHandler) Pending() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT u.id, COALESCE(ga.login, ''), COALESCE(ga.avatar_url, ''),
       u.kyc_status, u.updated_at::text, u.kyc_data,
       COALESCE(u.kyc_session_id, ''),
       -- The provider's short session counter, which their verification
       -- table displays. An identifier, not a description of anybody.
       COALESCE(u.kyc_data->>'session_number', ''),
       (SELECT count(*) FROM kyc_reset_audit r WHERE r.subject_user_id = u.id)
FROM users u
LEFT JOIN github_accounts ga ON ga.user_id = u.id
WHERE u.kyc_status IN ('in_review', 'rejected')
ORDER BY u.updated_at ASC
`)
		if err != nil {
			slog.Error("kyc pending: query failed", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "pending_failed"})
		}
		defer rows.Close()

		out := []fiber.Map{}
		for rows.Next() {
			var id uuid.UUID
			var login, avatarURL, status, updatedAt, sessionID, sessionNumber string
			var kycData []byte
			var resetCount int
			if err := rows.Scan(&id, &login, &avatarURL, &status, &updatedAt, &kycData, &sessionID, &sessionNumber, &resetCount); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "pending_failed"})
			}

			var decoded map[string]interface{}
			if len(kycData) > 0 {
				_ = json.Unmarshal(kycData, &decoded)
			}

			out = append(out, fiber.Map{
				"user_id":       id.String(),
				"github_login":  login,
				"avatar_url":    avatarURL,
				"kyc_status":    status,
				"waiting_since": updatedAt,
				// The provider's session identifier, and the only field from
				// their side this endpoint carries.
				//
				// It is an identifier, not data: no document, no decision, no
				// warning text. Without it a reviewer has to match a row to a
				// session in the provider console by GitHub username, which
				// the console does not index by and which is not unique to
				// anything it stores - so the match is done by eye, on the
				// screen where identity decisions are made.
				//
				// Empty after a reset, because Reset() nulls the column. That
				// is correct rather than lossy: the id of a detached session
				// is kept on the audit row (previous_session_id), and a row
				// with no live session is one nothing is waiting on.
				"kyc_session_id": sessionID,
				// The provider's own short session counter (3, 12, 41...).
				// This is the match key: their verification table displays it,
				// so a reviewer pairs a queue row with a session on the number
				// alone. It is why no personal detail is needed here.
				"session_number": sessionNumber,
				// How many times this person has been reset before. A second
				// or third reset is a different decision from a first, and an
				// admin should not have to open another screen to know which
				// one they are making.
				"previous_resets": resetCount,
				// May be empty - notably for a refusal whose only warnings are
				// ip_analysis, which map to nothing on purpose.
				"suggested_reason_codes": SuggestKYCReasons(decoded),
			})
		}
		if err := rows.Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "pending_failed"})
		}
		return c.JSON(fiber.Map{"pending": out})
	}
}
