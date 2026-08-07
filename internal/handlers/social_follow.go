package handlers

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// socialFollowPlatforms is the fixed set a user must have an approved
// submission for to complete the program. Keep in sync with the frontend's
// SOCIAL_FOLLOW_PLATFORMS list (features/settings/components/rewards).
var socialFollowPlatforms = []string{"github", "telegram", "linkedin"}

// socialFollowPointsPerCompletion is 5x referralPointsPerCompletion, per
// product decision: this program is worth noticeably more than a single
// referral since it requires action across three platforms.
const socialFollowPointsPerCompletion = 5 * referralPointsPerCompletion

// maxScreenshotDataURLBytes bounds the base64 data URL string
// (screenshot proof is stored as TEXT, same convention as
// ecosystems.logo_url) - roughly a 5MB image after base64's ~1.37x
// inflation, with headroom.
const maxScreenshotDataURLBytes = 8 * 1024 * 1024

func isValidSocialFollowPlatform(platform string) bool {
	for _, p := range socialFollowPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

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

type socialFollowSubmitRequest struct {
	Screenshot string `json:"screenshot"`
}

// Submit handles POST /social-follow/:platform/submit. Resubmission (e.g.
// after a rejection) overwrites the same row and resets it to pending.
func (h *SocialFollowHandler) Submit() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		platform := c.Params("platform")
		if !isValidSocialFollowPlatform(platform) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_platform"})
		}

		var req socialFollowSubmitRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if strings.TrimSpace(req.Screenshot) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "screenshot_required"})
		}
		if !strings.HasPrefix(req.Screenshot, "data:image/") {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "screenshot_must_be_an_image"})
		}
		if len(req.Screenshot) > maxScreenshotDataURLBytes {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "screenshot_too_large"})
		}

		_, err := h.db.Pool.Exec(c.Context(), `
INSERT INTO social_follow_submissions (user_id, platform, screenshot, status, rejection_reason, reviewed_by, reviewed_at, updated_at)
VALUES ($1, $2, $3, 'pending', NULL, NULL, NULL, now())
ON CONFLICT (user_id, platform) DO UPDATE SET
  screenshot = EXCLUDED.screenshot,
  status = 'pending',
  rejection_reason = NULL,
  reviewed_by = NULL,
  reviewed_at = NULL,
  updated_at = now()
`, userID, platform, req.Screenshot)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type socialFollowStatusDTO struct {
	Platform        string  `json:"platform"`
	Status          *string `json:"status"` // null when never submitted
	RejectionReason *string `json:"rejection_reason,omitempty"`
}

// Me handles GET /social-follow/me: per-platform submission status plus
// whether the combined reward has been earned.
func (h *SocialFollowHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT platform, status, rejection_reason FROM social_follow_submissions WHERE user_id = $1
`, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "status_lookup_failed"})
		}
		found := map[string]socialFollowStatusDTO{}
		for rows.Next() {
			var platform, status string
			var reason *string
			if err := rows.Scan(&platform, &status, &reason); err != nil {
				rows.Close()
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "status_scan_failed"})
			}
			found[platform] = socialFollowStatusDTO{Platform: platform, Status: &status, RejectionReason: reason}
		}
		rows.Close()

		out := make([]socialFollowStatusDTO, 0, len(socialFollowPlatforms))
		for _, p := range socialFollowPlatforms {
			if dto, ok := found[p]; ok {
				out = append(out, dto)
			} else {
				out = append(out, socialFollowStatusDTO{Platform: p, Status: nil})
			}
		}

		var pointsAwarded int
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT points_awarded FROM social_follow_completions WHERE user_id = $1
`, userID).Scan(&pointsAwarded)
		completed := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "completion_lookup_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"platforms":      out,
			"completed":      completed,
			"points_awarded": pointsAwarded,
			"points_reward":  socialFollowPointsPerCompletion,
		})
	}
}

type socialFollowSubmissionDTO struct {
	ID         uuid.UUID `json:"id"`
	UserID     uuid.UUID `json:"user_id"`
	Login      *string   `json:"login,omitempty"`
	Platform   string    `json:"platform"`
	Screenshot string    `json:"screenshot"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListSubmissions handles GET /admin/social-follow/submissions?status=pending
// (status defaults to "pending" - the review queue - but any valid status
// can be requested for a history view).
func (h *SocialFollowHandler) ListSubmissions() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		status := c.Query("status", "pending")

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT s.id, s.user_id, ga.login, s.platform, s.screenshot, s.status, s.created_at
FROM social_follow_submissions s
LEFT JOIN github_accounts ga ON ga.user_id = s.user_id
WHERE s.status = $1
ORDER BY s.created_at ASC
LIMIT 100
`, status)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submissions_list_failed"})
		}
		defer rows.Close()

		out := []socialFollowSubmissionDTO{}
		for rows.Next() {
			var s socialFollowSubmissionDTO
			if err := rows.Scan(&s.ID, &s.UserID, &s.Login, &s.Platform, &s.Screenshot, &s.Status, &s.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submissions_scan_failed"})
			}
			out = append(out, s)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"submissions": out})
	}
}

// Approve handles POST /admin/social-follow/submissions/:id/approve.
func (h *SocialFollowHandler) Approve() fiber.Handler {
	return func(c *fiber.Ctx) error {
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

		var submitterID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE social_follow_submissions
SET status = 'approved', reviewed_by = $1, reviewed_at = now(), updated_at = now()
WHERE id = $2
RETURNING user_id
`, adminID, submissionID).Scan(&submitterID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "submission_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "approve_failed"})
		}

		maybeCompleteSocialFollow(c.Context(), h.db, h.notify, submitterID)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type socialFollowRejectRequest struct {
	Reason string `json:"reason"`
}

// Reject handles POST /admin/social-follow/submissions/:id/reject.
func (h *SocialFollowHandler) Reject() fiber.Handler {
	return func(c *fiber.Ctx) error {
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
		var req socialFollowRejectRequest
		_ = c.BodyParser(&req) // reason is optional

		tag, err := h.db.Pool.Exec(c.Context(), `
UPDATE social_follow_submissions
SET status = 'rejected', rejection_reason = $1, reviewed_by = $2, reviewed_at = now(), updated_at = now()
WHERE id = $3
`, nullIfEmpty(req.Reason), adminID, submissionID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reject_failed"})
		}
		if tag.RowsAffected() == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "submission_not_found"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// maybeCompleteSocialFollow checks whether userID now has an approved
// submission for every platform, and if so awards the combined reward
// exactly once. Best-effort: never fails the caller (Approve() above).
func maybeCompleteSocialFollow(ctx context.Context, d *db.DB, notify *notifications.Service, userID uuid.UUID) {
	if d == nil || d.Pool == nil {
		return
	}

	var approvedCount int
	err := d.Pool.QueryRow(ctx, `
SELECT count(*) FROM social_follow_submissions WHERE user_id = $1 AND status = 'approved'
`, userID).Scan(&approvedCount)
	if err != nil {
		slog.Warn("social_follow: approved-count lookup failed", "user_id", userID, "error", err)
		return
	}
	if approvedCount < len(socialFollowPlatforms) {
		return
	}

	// PRIMARY KEY on user_id is the guard: only the first of any concurrent
	// approvals that crosses the threshold actually inserts a row.
	tag, err := d.Pool.Exec(ctx, `
INSERT INTO social_follow_completions (user_id, points_awarded)
VALUES ($1, $2)
ON CONFLICT (user_id) DO NOTHING
`, userID, socialFollowPointsPerCompletion)
	if err != nil {
		slog.Warn("social_follow: completion insert failed", "user_id", userID, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // already completed
	}

	if err := insertLedgerEntry(ctx, d.Pool, userID, socialFollowPointsPerCompletion, "social_follow", nil); err != nil {
		slog.Warn("social_follow: ledger entry failed", "user_id", userID, "error", err)
	}

	notify.Notify(ctx, userID, notifications.TypeSocialFollowCompleted,
		"Social follow reward earned",
		"You followed us on every platform and verification is complete - you earned "+strconv.Itoa(socialFollowPointsPerCompletion)+" points.",
		"/settings?tab=rewards",
	)
}
