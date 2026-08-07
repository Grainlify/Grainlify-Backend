package handlers

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// referralPointsPerCompletion is the flat point award a referrer earns once
// their referred user completes GitHub signup + KYC verification. Points
// have no ledger of their own - referrals.points_awarded on the completed
// row is the record; GetStats sums it at read time.
const referralPointsPerCompletion = 100

// referralCodeAlphabet avoids visually ambiguous characters (0/O, 1/I/L) so
// codes are easy to read and retype from a shared link.
const referralCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
const referralCodeLength = 8

type ReferralsHandler struct {
	db *db.DB
}

func NewReferralsHandler(d *db.DB) *ReferralsHandler {
	return &ReferralsHandler{db: d}
}

func (h *ReferralsHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// Me handles GET /referrals/me: the caller's referral code (generating one
// on first call) plus their referral counts and points earned.
func (h *ReferralsHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		code, err := ensureReferralCode(c.Context(), h.db, userID)
		if err != nil {
			slog.Error("referrals: failed to ensure referral code", "user_id", userID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "referral_code_failed"})
		}

		var totalReferred, pending, completed, pointsEarned int
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT
  count(*),
  count(*) FILTER (WHERE status = 'pending'),
  count(*) FILTER (WHERE status = 'completed'),
  COALESCE(sum(points_awarded) FILTER (WHERE status = 'completed'), 0)
FROM referrals
WHERE referrer_user_id = $1
`, userID).Scan(&totalReferred, &pending, &completed, &pointsEarned)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "referral_stats_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"code":              code,
			"total_referred":    totalReferred,
			"pending":           pending,
			"completed":         completed,
			"points_earned":     pointsEarned,
			"points_per_referral": referralPointsPerCompletion,
		})
	}
}

// ensureReferralCode returns userID's referral code, generating and
// persisting one on first call. Retries a handful of times on a unique
// constraint collision (astronomically unlikely at this code length, but
// cheap to handle correctly).
func ensureReferralCode(ctx context.Context, d *db.DB, userID uuid.UUID) (string, error) {
	var existing *string
	if err := d.Pool.QueryRow(ctx, `SELECT referral_code FROM users WHERE id = $1`, userID).Scan(&existing); err != nil {
		return "", err
	}
	if existing != nil && *existing != "" {
		return *existing, nil
	}

	for attempt := 0; attempt < 5; attempt++ {
		candidate := generateReferralCode()
		_, err := d.Pool.Exec(ctx, `UPDATE users SET referral_code = $1 WHERE id = $2`, candidate, userID)
		if err == nil {
			return candidate, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue // code collision, try another
		}
		return "", err
	}
	return "", fmt.Errorf("referrals: exhausted retries generating a unique referral code")
}

func generateReferralCode() string {
	b := make([]byte, referralCodeLength)
	_, _ = rand.Read(b)
	out := make([]byte, referralCodeLength)
	for i, v := range b {
		out[i] = referralCodeAlphabet[int(v)%len(referralCodeAlphabet)]
	}
	return string(out)
}

// attachReferral records a pending referral for a brand new user, called
// from github_oauth.go CallbackUnified right after that user's row is
// inserted. Best-effort: an invalid/unknown code or a failed insert must
// never block account creation, so errors are logged and swallowed.
func attachReferral(ctx context.Context, d *db.DB, refCode string, referredUserID uuid.UUID) {
	if d == nil || d.Pool == nil || refCode == "" {
		return
	}

	var referrerUserID uuid.UUID
	err := d.Pool.QueryRow(ctx, `SELECT id FROM users WHERE referral_code = $1`, refCode).Scan(&referrerUserID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("referrals: referrer lookup failed", "ref_code", refCode, "error", err)
		}
		return
	}
	if referrerUserID == referredUserID {
		return // defensive: a user cannot refer themselves
	}

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO referrals (referrer_user_id, referred_user_id, status)
VALUES ($1, $2, 'pending')
`, referrerUserID, referredUserID); err != nil {
		slog.Warn("referrals: failed to record pending referral", "referrer_user_id", referrerUserID, "referred_user_id", referredUserID, "error", err)
	}
}

// maybeCompleteReferral is called after a user's KYC status transitions to
// "verified" (from kyc.go Status() and didit_webhook.go Receive()). If this
// user was referred and their referral is still pending, marks it completed
// and notifies the referrer. Best-effort: never fails the caller - KYC
// verification must succeed regardless of what happens here.
func maybeCompleteReferral(ctx context.Context, d *db.DB, notify *notifications.Service, referredUserID uuid.UUID) {
	if d == nil || d.Pool == nil {
		return
	}

	var referralID, referrerUserID uuid.UUID
	err := d.Pool.QueryRow(ctx, `
SELECT id, referrer_user_id FROM referrals
WHERE referred_user_id = $1 AND status = 'pending'
`, referredUserID).Scan(&referralID, &referrerUserID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("referrals: pending-referral lookup failed", "referred_user_id", referredUserID, "error", err)
		}
		return
	}

	// Guards against a race between the webhook and the status-poll path
	// both observing the "verified" transition at nearly the same time -
	// only one Exec wins the pending->completed move.
	tag, err := d.Pool.Exec(ctx, `
UPDATE referrals
SET status = 'completed', points_awarded = $1, completed_at = now()
WHERE id = $2 AND status = 'pending'
`, referralPointsPerCompletion, referralID)
	if err != nil {
		slog.Warn("referrals: completion update failed", "referral_id", referralID, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return
	}

	notify.Notify(ctx, referrerUserID, notifications.TypeReferralCompleted,
		"Referral reward earned",
		fmt.Sprintf("Someone you referred completed verification. You earned %d points.", referralPointsPerCompletion),
		"/settings?tab=referrals",
	)
}
