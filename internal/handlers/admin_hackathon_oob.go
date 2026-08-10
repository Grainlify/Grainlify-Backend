package handlers

import (
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// AdminHackathonOOBHandler surfaces out-of-band assignments in the admin
// dashboard (AI-specs.md §2.3 step 4).
type AdminHackathonOOBHandler struct {
	db *db.DB
}

func NewAdminHackathonOOBHandler(d *db.DB) *AdminHackathonOOBHandler {
	return &AdminHackathonOOBHandler{db: d}
}

// List handles GET /admin/hackathons/:id/oob-assignments.
//
// Returns per-org totals with the individual events attached, because a bare
// count does not support the decision this feeds. "3 out-of-band assignments"
// is unactionable; "3 assignments across 3 issues, all to the same unfamiliar
// account, none reverted" and "3 assignments to 3 different newcomers, all
// reverted" are the same number and completely different situations.
func (h *AdminHackathonOOBHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		summaries, err := hackathon.OOBOrgSummaries(c.Context(), h.db.Pool, hackathonID)
		if err != nil {
			slog.Error("oob summaries", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "oob_fetch_failed"})
		}

		for i := range summaries {
			events, err := hackathon.OOBAssignmentsForOrg(c.Context(), h.db.Pool, hackathonID, summaries[i].OrgLogin)
			if err != nil {
				slog.Warn("oob events", "org", summaries[i].OrgLogin, "error", err)
				continue
			}
			summaries[i].Assignments = events
		}

		return c.JSON(fiber.Map{
			"orgs": summaries,
			// Stated in the payload so the UI does not have to encode this
			// rule itself, and so it stays true if the UI is rewritten.
			// §7 makes eligibility reductions admin-reviewable, not automatic.
			"flagging_is_advisory": true,
			"note": "Crossing the threshold flags an org for review. It applies no penalty and " +
				"withholds no payment on its own - repeated out-of-band assignment can be a " +
				"maintainer who has not read the rules or one routing issues to an alt account, " +
				"and those are indistinguishable from this data alone.",
		})
	}
}
