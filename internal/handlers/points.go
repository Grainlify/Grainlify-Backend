package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// pgExecutor is the subset of DBPool that point_ledger inserts need. Both
// db.DBPool (outside a transaction) and pgx.Tx (inside one - see
// redemptions.go's atomic deduct-and-record) satisfy it structurally, so
// insertLedgerEntry works either way without callers needing an interface
// conversion.
type pgExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// insertLedgerEntry records one point_ledger row. amount is positive for a
// grant (referral, social-follow) or negative for a spend/reversal
// (redemption request, rejected-redemption refund). referenceID is the
// causing row's id (referrals.id, social_follow_completions.user_id-based
// lookups don't apply here since that table's PK is user_id, redemptions.id)
// and may be nil.
func insertLedgerEntry(ctx context.Context, exec pgExecutor, userID uuid.UUID, amount int, reason string, referenceID *uuid.UUID) error {
	_, err := exec.Exec(ctx, `
INSERT INTO point_ledger (user_id, amount, reason, reference_id)
VALUES ($1, $2, $3, $4)
`, userID, amount, reason, referenceID)
	return err
}

// pointsBalance returns userID's current spendable point balance: the sum
// of every point_ledger entry (earns positive, spends/reversals negative).
func pointsBalance(ctx context.Context, d *db.DB, userID uuid.UUID) (int, error) {
	var balance int
	err := d.Pool.QueryRow(ctx, `
SELECT COALESCE(sum(amount), 0) FROM point_ledger WHERE user_id = $1
`, userID).Scan(&balance)
	return balance, err
}

type PointsHandler struct {
	db *db.DB
}

func NewPointsHandler(d *db.DB) *PointsHandler {
	return &PointsHandler{db: d}
}

func (h *PointsHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// Me handles GET /points/me: the caller's current spendable point balance.
func (h *PointsHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		balance, err := pointsBalance(c.Context(), h.db, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "balance_lookup_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"balance":               balance,
			"usdc_per_point":        usdcPerPoint,
			"min_redemption_points": minRedemptionPoints,
		})
	}
}
