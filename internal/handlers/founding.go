package handlers

import (
	"context"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/founding"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// onVerifiedForFoundingPool bridges identity verification to the Founding
// Contributor Pool.
//
// Best-effort throughout: verification must succeed whatever happens here, so
// every failure is logged and swallowed rather than returned. A missed share
// is recoverable from source data - the ledger is reconstructable by design -
// whereas a failed verification is a user who cannot proceed at all.
func onVerifiedForFoundingPool(ctx context.Context, d *db.DB, userID uuid.UUID) {
	if d == nil || d.Pool == nil {
		return
	}
	cfg, err := hackathon.EffectiveValues(ctx, d.Pool, nil)
	if err != nil {
		slog.Warn("founding: config load failed", "user_id", userID, "error", err)
		return
	}
	founding.OnVerified(ctx, d.Pool, userID, cfg)
}

// FoundingHandler serves the public wave progress.
//
// Note what is *not* here: no endpoint returns a computed payout, a share
// value, or any per-person figure. §6 forbids publishing any per-person
// number, and a settlement result reaching a profile page or a notification
// would publish exactly that. Settlement results are stored and rendered
// nowhere - see the guard test in founding_no_ui_test.go, which fails if an
// endpoint ever starts reading them.
type FoundingHandler struct {
	db *db.DB
}

func NewFoundingHandler(d *db.DB) *FoundingHandler { return &FoundingHandler{db: d} }

// WaveProgress handles GET /founding/waves.
//
// Reports slots **claimed**, never remaining. A remaining-count on a small
// community advertises emptiness; a rising claimed-count reads as momentum.
// Same data, opposite signal - and it is the same reasoning that has draw
// applicant counts bucketed rather than exact.
func (h *FoundingHandler) WaveProgress() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		cfg, err := hackathon.EffectiveValues(c.Context(), h.db.Pool, nil)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "config_load_failed"})
		}
		p, err := founding.Progress(c.Context(), h.db.Pool, cfg)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "wave_progress_failed"})
		}
		return c.JSON(p)
	}
}

// MyShares handles GET /founding/me: the caller's own share breakdown.
//
// Shares only - never a converted amount. A share count is a fact about what
// somebody did; a dollar figure is a promise about what they will receive,
// and until settlement runs nobody can honestly make one.
func (h *FoundingHandler) MyShares() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		b, err := founding.BreakdownFor(c.Context(), h.db.Pool, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "shares_lookup_failed"})
		}
		m, hasMembership, err := founding.MembershipFor(c.Context(), h.db.Pool, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "membership_lookup_failed"})
		}
		resp := fiber.Map{"shares": b, "member": hasMembership}
		if hasMembership {
			resp["wave"] = m.Wave
			resp["multiplier"] = m.Multiplier
			resp["sequence_number"] = m.Sequence
		}
		return c.JSON(resp)
	}
}

func (h *FoundingHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
