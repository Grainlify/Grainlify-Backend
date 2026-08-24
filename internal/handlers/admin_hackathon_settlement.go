package handlers

import (
	"errors"
	"log/slog"
	"math/big"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// AdminHackathonSettlementHandler shows what an event would pay, before it
// pays it.
type AdminHackathonSettlementHandler struct {
	db *db.DB
}

func NewAdminHackathonSettlementHandler(d *db.DB) *AdminHackathonSettlementHandler {
	return &AdminHackathonSettlementHandler{db: d}
}

type settlementLineDTO struct {
	UserID string `json:"user_id"`
	// RawWeight and Multiplier are the two exact stored values the weight is
	// built from - units and curve_multiplier - so a reader can check the
	// arithmetic rather than trust it.
	RawWeight       string `json:"raw_weight"`
	Multiplier      string `json:"multiplier"`
	EffectiveWeight string `json:"effective_weight"`
	// AmountMinor is the integer that would go into a Merkle leaf. The decimal
	// beside it is for reading only; nothing computes from it.
	AmountMinor string `json:"amount_minor"`
	AmountUSDC  string `json:"amount_usdc"`
}

type settlementPreviewDTO struct {
	HackathonID string `json:"hackathon_id"`
	Pool        string `json:"pool"`
	ChainID     string `json:"chain_id"`
	PoolMinor   string `json:"pool_minor"`
	PoolUSDC    string `json:"pool_usdc"`
	// TotalWeight is the divisor. Shown because every line's share is
	// weight/total, and a reader who cannot see the denominator cannot check a
	// single row.
	TotalWeight string `json:"total_weight"`
	LineCount   int    `json:"line_count"`
	// PayableCount excludes lines that rounded to zero. They stay in Lines:
	// "you were in this event and received nothing" is a fact worth showing,
	// and dropping those rows would make the totals right and the record wrong.
	PayableCount   int                 `json:"payable_count"`
	AllocatedMinor string              `json:"allocated_minor"`
	SumsToPool     bool                `json:"sums_to_pool"`
	Lines          []settlementLineDTO `json:"lines"`
	AlreadySettled bool                `json:"already_settled"`
	SettlementID   *string             `json:"settlement_id"`
}

// Preview handles GET /admin/hackathons/:id/settlement-preview?pool=contributor
//
// Computes the settlement and **writes nothing**. This is the readable artefact
// before the act.
//
// WHAT PERSISTS IT, corrected. This comment used to say "the transition to
// `settled` is what actually persists". It does not, and never did. Transition
// runs CloseAppealsAndRecompute and SettleMaintainerPool; neither writes a
// settlement row. Nothing in any request path calls settlement.Persist.
//
// That mattered more than an ordinary stale comment, because of where it sat:
// an admin reads this immediately before moving an event to `settled`, and it
// told them the action recorded something it did not. The settlement existed
// only as this preview until somebody ran the CLI.
//
// Recording a hackathon settlement is `payout persist --hackathon <id> --pool
// <kind>`, deliberately outside the API - it creates the id every later step
// keys to, and the acknowledgement gate that follows is a person reading a
// report and restating a figure, which is not a thing an HTTP call does well.
// See docs/RUNBOOK-founding-payout.md; the sequence is the same for both
// producers.
//
// It deliberately re-computes rather than reading back a stored settlement. A
// preview of what is already recorded would answer a different question - "what
// did we do" instead of "what would we do" - and the second is the one being
// asked before the act. When a settlement already exists, that is reported
// alongside, because recomputing after the fact is how a dispute gets answered.
func (h *AdminHackathonSettlementHandler) Preview() fiber.Handler {
	return func(c *fiber.Ctx) error {
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid hackathon id"})
		}
		poolKind := c.Query("pool", "contributor")
		chainID := c.Query("chain_id", "")

		res, err := hackathon.SettlementFor(c.Context(), h.db.Pool, hackathonID, poolKind, chainID)
		if errors.Is(err, hackathon.ErrNothingToSettle) {
			// Not an error. An event where every submission was rejected, or
			// whose pool is unset, settles to nothing - and saying so is the
			// answer, not a failure to produce one.
			return c.JSON(fiber.Map{
				"hackathon_id":      hackathonID.String(),
				"pool":              poolKind,
				"nothing_to_settle": true,
				"reason":            err.Error(),
			})
		}
		if err != nil {
			slog.Error("hackathon settlement preview", "error", err, "hackathon_id", hackathonID, "pool", poolKind)
			return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
		}

		// Asked of internal/hackathon rather than queried here. A settlement
		// table read from a presentation package is banned by
		// TestSettlementFiguresNeverReachAPresentationLayer, and the ban is
		// right: a handler holding that query is one edit from rendering a
		// per-person figure out of it.
		var existingID *string
		if id, _ := hackathon.ExistingSettlementID(c.Context(), h.db.Pool, hackathonID, poolKind); id != nil {
			s := id.String()
			existingID = &s
		}

		lines := make([]settlementLineDTO, 0, len(res.Lines))
		for _, l := range res.Lines {
			lines = append(lines, settlementLineDTO{
				UserID:          l.UserID.String(),
				RawWeight:       l.RawWeight.RatString(),
				Multiplier:      l.Multiplier.RatString(),
				EffectiveWeight: l.EffectiveWeight.RatString(),
				AmountMinor:     l.AmountMinor.String(),
				AmountUSDC:      minorToUSDC(l.AmountMinor),
			})
		}

		allocated := res.TotalAllocatedMinor()
		return c.JSON(settlementPreviewDTO{
			HackathonID:    hackathonID.String(),
			Pool:           res.Pool,
			ChainID:        res.ChainID,
			PoolMinor:      res.PoolMinor.String(),
			PoolUSDC:       minorToUSDC(res.PoolMinor),
			TotalWeight:    res.TotalEffective.RatString(),
			LineCount:      len(res.Lines),
			PayableCount:   len(res.PayableLines()),
			AllocatedMinor: allocated.String(),
			SumsToPool:     allocated.Cmp(res.PoolMinor) == 0,
			Lines:          lines,
			AlreadySettled: existingID != nil,
			SettlementID:   existingID,
		})
	}
}

// minorToUSDC renders minor units for reading only.
func minorToUSDC(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return new(big.Rat).SetFrac(v, big.NewInt(1_000_000)).FloatString(6)
}
