package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Following Grainlify is an **eligibility requirement** for the Founding
// Contributor Pool, worth zero shares.
//
// It used to pay 500 points. Paying real money for a free, reversible action
// a bot can perform was the same mistake the whole redesign exists to undo -
// and it came out of the budget that now pays people who ship code. As a
// requirement it costs nothing, gets more follows rather than fewer (everyone
// who wants a share must follow), and cannot be farmed, because there is
// nothing to collect.
//
// Nothing in this file grants anything. If a future change adds a reward
// here, it is reintroducing the retired programme.

// socialFollowPlatforms is the set a contributor must submit proof for. Both
// are required and both are submitted together - see SubmitAll.
//
// Keep in sync with the frontend's SOCIAL_FOLLOW_PLATFORMS.
var socialFollowPlatforms = []string{"linkedin", "x"}

// maxScreenshotDataURLBytes bounds each base64 data URL string (screenshots
// are stored as TEXT, same convention as ecosystems.logo_url) - roughly a 5MB
// image after base64's ~1.37x inflation, with headroom.
const maxScreenshotDataURLBytes = 8 * 1024 * 1024

// Submission states. 'revoked' is deliberately distinct from 'rejected':
// rejected means the proof was not good enough, revoked means it was accepted
// and has since been withdrawn. Only the second needs explaining to somebody
// who thought they were eligible.
const (
	socialFollowPending  = "pending"
	socialFollowApproved = "approved"
	socialFollowRejected = "rejected"
	socialFollowRevoked  = "revoked"
)

type SocialFollowHandler struct {
	db     *db.DB
	notify *notifications.Service
}

func NewSocialFollowHandler(d *db.DB, notify *notifications.Service) *SocialFollowHandler {
	return &SocialFollowHandler{db: d, notify: notify}
}

func (h *SocialFollowHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func isImageDataURL(s string) bool {
	return strings.HasPrefix(s, "data:image/")
}

type socialFollowSubmitRequest struct {
	LinkedIn string `json:"linkedin_screenshot"`
	X        string `json:"x_screenshot"`
}

// SubmitAll handles POST /social-follow/submit: both platforms, one request,
// all or nothing.
//
// Atomicity is the point. Under the previous per-platform endpoint a
// contributor could be approved on one platform and pending on another, which
// meant "are they eligible?" had no single answer and a reviewer had to make
// two decisions that only made sense together. Both screenshots arrive in one
// request, land in one row, and are decided once.
//
// Resubmission after a rejection or a revocation overwrites the same row and
// returns it to pending. Nothing is lost by that: every decision ever taken is
// in social_follow_decisions, which is what a dispute is answered from.
func (h *SocialFollowHandler) SubmitAll() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req socialFollowSubmitRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		linkedIn, x := strings.TrimSpace(req.LinkedIn), strings.TrimSpace(req.X)

		// Validate both before writing either. Refusing the whole request is
		// the entire reason this endpoint exists: accepting the valid half
		// would recreate the partial state the atomic model removes.
		if linkedIn == "" || x == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "both_screenshots_required",
				"message": "Both platforms are submitted together. Upload a screenshot for each before submitting.",
			})
		}
		for _, s := range []string{linkedIn, x} {
			if !isImageDataURL(s) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_screenshot"})
			}
			if len(s) > maxScreenshotDataURLBytes {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "screenshot_too_large"})
			}
		}

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}
		defer func() { _ = tx.Rollback(c.Context()) }()

		var submissionID uuid.UUID
		if err := tx.QueryRow(c.Context(), `
INSERT INTO social_follow_submissions (user_id, linkedin_screenshot, x_screenshot, status)
VALUES ($1, $2, $3, 'pending')
ON CONFLICT (user_id) DO UPDATE
SET linkedin_screenshot = EXCLUDED.linkedin_screenshot,
    x_screenshot        = EXCLUDED.x_screenshot,
    status              = 'pending',
    decided_by          = NULL,
    decided_at          = NULL,
    decision_reason     = NULL,
    updated_at          = now()
RETURNING id
`, userID, linkedIn, x).Scan(&submissionID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}

		if _, err := tx.Exec(c.Context(), `
INSERT INTO social_follow_decisions (submission_id, decision, actor_user_id)
VALUES ($1, 'submitted', $2)
`, submissionID, userID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}
		return c.JSON(fiber.Map{"id": submissionID, "status": socialFollowPending})
	}
}

// Me handles GET /social-follow/me.
//
// Returns the decision and its reason, including for a revocation. Somebody
// whose eligibility is withdrawn without an explanation reads it as
// arbitrary, and they would be right to.
func (h *SocialFollowHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var (
			status  string
			reason  *string
			decided *string
		)
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT status, decision_reason, decided_at::text
FROM social_follow_submissions WHERE user_id = $1
`, userID).Scan(&status, &reason, &decided)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(fiber.Map{
				"platforms": socialFollowPlatforms,
				"submitted": false,
				"status":    nil,
				"eligible":  false,
			})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
		}

		return c.JSON(fiber.Map{
			"platforms":       socialFollowPlatforms,
			"submitted":       true,
			"status":          status,
			"decision_reason": reason,
			"decided_at":      decided,
			// The single question everything else here exists to answer.
			"eligible": status == socialFollowApproved,
		})
	}
}

// ListSubmissions handles GET /admin/social-follow/submissions.
//
// Returns both screenshots so a reviewer can see them side by side and make
// one decision, rather than judging one platform without the other in view.
func (h *SocialFollowHandler) ListSubmissions() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		// Defaults to the review queue; ?status=all for the full picture,
		// which is what a revocation is decided from.
		filter := c.Query("status", socialFollowPending)
		query := `
SELECT s.id, s.user_id, COALESCE(ga.login, ''), s.linkedin_screenshot, s.x_screenshot,
       s.status, s.decision_reason, s.decided_at::text, s.created_at::text
FROM social_follow_submissions s
LEFT JOIN github_accounts ga ON ga.user_id = s.user_id
`
		args := []any{}
		if filter != "all" {
			query += " WHERE s.status = $1"
			args = append(args, filter)
		}
		query += " ORDER BY s.created_at ASC"

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []fiber.Map{}
		for rows.Next() {
			var (
				id, userID                 uuid.UUID
				login, linkedIn, x, status string
				reason, decidedAt          *string
				createdAt                  string
			)
			if err := rows.Scan(&id, &userID, &login, &linkedIn, &x, &status, &reason, &decidedAt, &createdAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, fiber.Map{
				"id": id, "user_id": userID, "github_login": login,
				"linkedin_screenshot": linkedIn, "x_screenshot": x,
				"status": status, "decision_reason": reason,
				"decided_at": decidedAt, "created_at": createdAt,
			})
		}
		return c.JSON(fiber.Map{"submissions": out})
	}
}

type socialFollowDecisionRequest struct {
	Reason string `json:"reason"`
}

// decide applies one decision to a whole submission and logs it.
//
// One decision covers both platforms by construction - there is no per-
// platform state left to disagree with itself.
func (h *SocialFollowHandler) decide(c *fiber.Ctx, to string, requireReason bool) error {
	if h.db == nil || h.db.Pool == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
	}
	adminID, ok := h.userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
	}
	submissionID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_submission_id"})
	}

	var req socialFollowDecisionRequest
	_ = c.BodyParser(&req)
	reason := strings.TrimSpace(req.Reason)
	if requireReason && reason == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":   "reason_required",
			"message": "A reason is recorded and shown to the contributor, so this decision needs one.",
		})
	}

	tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "decision_failed"})
	}
	defer func() { _ = tx.Rollback(c.Context()) }()

	// Revocation applies only to something currently approved. Revoking a
	// pending or rejected submission is meaningless and almost certainly a
	// misclick on the wrong row.
	var submitterID uuid.UUID
	q := `
UPDATE social_follow_submissions
SET status = $1, decided_by = $2, decided_at = now(), decision_reason = NULLIF($3, ''), updated_at = now()
WHERE id = $4`
	if to == socialFollowRevoked {
		q += ` AND status = '` + socialFollowApproved + `'`
	}
	q += ` RETURNING user_id`

	if err := tx.QueryRow(c.Context(), q, to, adminID, reason, submissionID).Scan(&submitterID); errors.Is(err, pgx.ErrNoRows) {
		if to == socialFollowRevoked {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":   "not_approved",
				"message": "Only an approved submission can be revoked.",
			})
		}
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "submission_not_found"})
	} else if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "decision_failed"})
	}

	if _, err := tx.Exec(c.Context(), `
INSERT INTO social_follow_decisions (submission_id, decision, reason, actor_user_id)
VALUES ($1, $2, NULLIF($3, ''), $4)
`, submissionID, to, reason, adminID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "decision_failed"})
	}

	if err := tx.Commit(c.Context()); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "decision_failed"})
	}

	h.notifyDecision(c, submitterID, to, reason)
	return c.JSON(fiber.Map{"ok": true, "status": to})
}

// notifyDecision tells the contributor what happened. Best-effort: a failed
// notification must not undo a recorded decision.
//
// A revocation is the one that matters. Eligibility disappearing silently,
// and only becoming visible when the pool is shared out, is exactly how a
// fair decision comes to look arbitrary.
func (h *SocialFollowHandler) notifyDecision(c *fiber.Ctx, userID uuid.UUID, to, reason string) {
	if h.notify == nil {
		return
	}
	var title, body string
	switch to {
	case socialFollowApproved:
		title = "Social follow approved"
		body = "Your follow proof was approved. You're eligible for the Founding Contributor Pool."
	case socialFollowRejected:
		title = "Social follow proof needs another look"
		body = "Your follow proof wasn't approved: " + reason + " You can upload new screenshots and submit again."
	case socialFollowRevoked:
		title = "Social follow eligibility withdrawn"
		body = "Your follow approval was withdrawn: " + reason + " You can submit new proof to become eligible again."
	default:
		return
	}
	h.notify.Notify(c.Context(), userID, notifications.TypeSocialFollowCompleted, title, body, "/settings?subtab=rewards")
}

// Approve handles POST /admin/social-follow/submissions/:id/approve.
func (h *SocialFollowHandler) Approve() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowApproved, false) }
}

// Reject handles POST /admin/social-follow/submissions/:id/reject. A reason is
// required - it is shown to the contributor so they can fix and resubmit.
func (h *SocialFollowHandler) Reject() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowRejected, true) }
}

// Revoke handles POST /admin/social-follow/submissions/:id/revoke.
//
// Eligibility is re-read at settlement, so an approval must be withdrawable
// after the fact. The submission and both screenshots are kept: this is a
// status change, and the record of what was approved is precisely what a
// revocation dispute turns on.
func (h *SocialFollowHandler) Revoke() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowRevoked, true) }
}
