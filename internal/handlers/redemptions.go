package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stellar/go/strkey"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// usdcPerPoint and minRedemptionPoints are a starting product default, not
// a value derived from anywhere else - flagged for the operator to revisit.
// At this rate: a completed referral (100 points) = $1, the social-follow
// program (500 points) = $5.
const usdcPerPoint = 0.01
const minRedemptionPoints = 100

// Actually sending USDC is intentionally NOT wired here - there is no
// funded treasury Stellar account configured yet (see internal/soroban,
// which is bounty-escrow shaped, not a redeem-on-demand primitive anyway).
// A redemption is created 'pending' with points deducted immediately (so
// the same points can't fund two simultaneous requests) and an admin sends
// the USDC manually, then marks it paid or rejects it (refunding the
// points) via the endpoints below.

type RedemptionsHandler struct {
	db     *db.DB
	notify *notifications.Service
}

func NewRedemptionsHandler(d *db.DB, notify *notifications.Service) *RedemptionsHandler {
	return &RedemptionsHandler{db: d, notify: notify}
}

func (h *RedemptionsHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

type redemptionDTO struct {
	ID            uuid.UUID `json:"id"`
	PointsSpent   int       `json:"points_spent"`
	USDCAmount    string    `json:"usdc_amount"`
	WalletAddress string    `json:"stellar_wallet_address"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

type createRedemptionRequest struct {
	Points               int    `json:"points"`
	StellarWalletAddress string `json:"stellar_wallet_address"`
}

// Create handles POST /redemptions. Deducts the requested points and
// records the request in one transaction - both happen or neither does.
func (h *RedemptionsHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req createRedemptionRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if req.Points < minRedemptionPoints {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "below_minimum_redemption",
				"minimum": minRedemptionPoints,
			})
		}
		wallet := strings.TrimSpace(req.StellarWalletAddress)
		if !strkey.IsValidEd25519PublicKey(wallet) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_stellar_wallet_address"})
		}

		balance, err := pointsBalance(c.Context(), h.db, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "balance_lookup_failed"})
		}
		if req.Points > balance {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "insufficient_balance", "balance": balance})
		}

		usdcAmount := float64(req.Points) * usdcPerPoint

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemption_create_failed"})
		}
		defer tx.Rollback(c.Context())

		var redemptionID uuid.UUID
		err = tx.QueryRow(c.Context(), `
INSERT INTO redemptions (user_id, points_spent, usdc_amount, stellar_wallet_address, status)
VALUES ($1, $2, $3, $4, 'pending')
RETURNING id
`, userID, req.Points, usdcAmount, wallet).Scan(&redemptionID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemption_create_failed"})
		}

		if err := insertLedgerEntry(c.Context(), tx, userID, -req.Points, "redemption", &redemptionID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemption_create_failed"})
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemption_create_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"id": redemptionID, "usdc_amount": usdcAmount})
	}
}

// Mine handles GET /redemptions/me: the caller's own redemption history.
func (h *RedemptionsHandler) Mine() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, points_spent, usdc_amount::text, stellar_wallet_address, status, created_at
FROM redemptions WHERE user_id = $1 ORDER BY created_at DESC LIMIT 100
`, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemptions_list_failed"})
		}
		defer rows.Close()

		out := []redemptionDTO{}
		for rows.Next() {
			var r redemptionDTO
			if err := rows.Scan(&r.ID, &r.PointsSpent, &r.USDCAmount, &r.WalletAddress, &r.Status, &r.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemptions_scan_failed"})
			}
			out = append(out, r)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"redemptions": out})
	}
}

type adminRedemptionDTO struct {
	redemptionDTO
	UserID uuid.UUID `json:"user_id"`
	Login  *string   `json:"login,omitempty"`
}

// ListAdmin handles GET /admin/redemptions?status=pending.
func (h *RedemptionsHandler) ListAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		status := c.Query("status", "pending")

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT r.id, r.user_id, ga.login, r.points_spent, r.usdc_amount::text, r.stellar_wallet_address, r.status, r.created_at
FROM redemptions r
LEFT JOIN github_accounts ga ON ga.user_id = r.user_id
WHERE r.status = $1
ORDER BY r.created_at ASC
LIMIT 100
`, status)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemptions_list_failed"})
		}
		defer rows.Close()

		out := []adminRedemptionDTO{}
		for rows.Next() {
			var r adminRedemptionDTO
			if err := rows.Scan(&r.ID, &r.UserID, &r.Login, &r.PointsSpent, &r.USDCAmount, &r.WalletAddress, &r.Status, &r.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "redemptions_scan_failed"})
			}
			out = append(out, r)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"redemptions": out})
	}
}

// MarkPaid handles POST /admin/redemptions/:id/mark-paid, recording that an
// admin sent the USDC manually (see the package comment above).
func (h *RedemptionsHandler) MarkPaid() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		adminID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		redemptionID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_redemption_id"})
		}

		var recipientID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE redemptions
SET status = 'paid', reviewed_by = $1, reviewed_at = now()
WHERE id = $2 AND status = 'pending'
RETURNING user_id
`, adminID, redemptionID).Scan(&recipientID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "redemption_not_found_or_not_pending"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "mark_paid_failed"})
		}

		h.notify.Notify(c.Context(), recipientID, notifications.TypeRedemptionPaid,
			"Redemption paid",
			"Your points redemption has been paid out.",
			"/settings?tab=rewards",
		)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type rejectRedemptionRequest struct {
	Reason string `json:"reason"`
}

// Reject handles POST /admin/redemptions/:id/reject, refunding the spent
// points via a reversing ledger entry.
func (h *RedemptionsHandler) Reject() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		adminID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		redemptionID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_redemption_id"})
		}
		var req rejectRedemptionRequest
		_ = c.BodyParser(&req) // reason is optional

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reject_failed"})
		}
		defer tx.Rollback(c.Context())

		var recipientID uuid.UUID
		var pointsSpent int
		err = tx.QueryRow(c.Context(), `
UPDATE redemptions
SET status = 'rejected', admin_note = $1, reviewed_by = $2, reviewed_at = now()
WHERE id = $3 AND status = 'pending'
RETURNING user_id, points_spent
`, nullIfEmpty(req.Reason), adminID, redemptionID).Scan(&recipientID, &pointsSpent)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "redemption_not_found_or_not_pending"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reject_failed"})
		}

		if err := insertLedgerEntry(c.Context(), tx, recipientID, pointsSpent, "redemption_reversal", &redemptionID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reject_failed"})
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "reject_failed"})
		}

		h.notify.Notify(c.Context(), recipientID, notifications.TypeRedemptionRejected,
			"Redemption rejected",
			"Your points redemption was rejected and the points have been refunded to your balance.",
			"/settings?tab=rewards",
		)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
