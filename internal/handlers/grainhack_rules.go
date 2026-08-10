package handlers

import (
	"encoding/json"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// GrainHackRulesHandler serves the public rules page's data.
//
// AI-specs.md §4.5 and §12 both require the rules be published before an
// event ("Publish the weights on the platform before the event"; "Publish
// before each event starts: bucket definitions, draw weights, hard gates,
// payout floor rule"). This endpoint exists so that page can render from
// what is actually running rather than from a hand-maintained document,
// which would drift the first time anyone changed a value.
type GrainHackRulesHandler struct {
	db *db.DB
}

func NewGrainHackRulesHandler(d *db.DB) *GrainHackRulesHandler {
	return &GrainHackRulesHandler{db: d}
}

type ruleDTO struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Type        string `json:"type"`
	Section     string `json:"section"`
	Description string `json:"description"`
	ValidRange  string `json:"valid_range,omitempty"`
	// Active is false for rules that are configured but have no consuming
	// logic yet. Published anyway - hiding them would make the page look
	// complete when it isn't - but flagged so nobody plans around a value
	// that currently does nothing.
	Active bool `json:"active"`
}

// Rules handles GET /grainhack/rules?hackathon_id=. Public, no auth: these
// are the rules contributors are expected to read before applying.
//
// For a hackathon that has gone live, this reads its §1.1 config snapshot,
// not live config - that snapshot is what the running event actually
// obeys, so publishing anything else would be publishing a lie.
func (h *GrainHackRulesHandler) Rules() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		var hackathonID *uuid.UUID
		var hackathonName, phase string
		source := "global_defaults"
		values := map[string]string{}

		if raw := c.Query("hackathon_id"); raw != "" {
			id, err := uuid.Parse(raw)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
			}
			var snapshot []byte
			if err := h.db.Pool.QueryRow(c.Context(), `
SELECT name, phase, config_snapshot FROM hackathons WHERE id = $1 AND phase <> 'draft'
`, id).Scan(&hackathonName, &phase, &snapshot); err != nil {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
			}
			hackathonID = &id

			if len(snapshot) > 0 && json.Unmarshal(snapshot, &values) == nil && len(values) > 0 {
				// The event is live or closed and reads its own frozen copy.
				source = "snapshot"
			} else {
				// Not yet frozen: show what it would run with today, and say so.
				resolved, err := hackathon.EffectiveValues(c.Context(), h.db.Pool, hackathonID)
				if err != nil {
					slog.Error("grainhack rules: effective values", "error", err)
					return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "rules_failed"})
				}
				values = resolved
				source = "not_yet_frozen"
			}
		} else {
			resolved, err := hackathon.EffectiveValues(c.Context(), h.db.Pool, nil)
			if err != nil {
				slog.Error("grainhack rules: global values", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "rules_failed"})
			}
			values = resolved
		}

		rules := make([]ruleDTO, 0, len(values))
		for key, value := range values {
			def, ok := hackathon.Definitions[key]
			if !ok {
				// A snapshot from an older code version can contain keys that
				// no longer exist. Publish them rather than hiding them - the
				// event really did run with them.
				rules = append(rules, ruleDTO{Key: key, Value: value, Section: "Other", Active: false})
				continue
			}
			rules = append(rules, ruleDTO{
				Key: key, Value: value, Type: def.Type, Section: def.Section,
				Description: def.Description, ValidRange: def.ValidRange, Active: def.Active,
			})
		}

		return c.JSON(fiber.Map{
			"source":         source,
			"hackathon_id":   hackathonID,
			"hackathon_name": hackathonName,
			"phase":          phase,
			"rules":          rules,
			"section_order":  hackathon.SectionOrder,
			// Read alongside the numbers, not instead of them. The maintainer
			// note exists because the pool split looks narrower than the
			// underlying contributor counts and that difference is easy to
			// misread as the scoring not working.
			"section_notes": fiber.Map{
				"Maintainer pool": "Two of these are floor criteria rather than growth measures: whether the repo " +
					"had commits before the event was announced, and whether it stayed active afterwards. A repo that " +
					"has been around and stays around scores full marks on both, so most participating repos sit near " +
					"the top of them. They exist to separate a real project from one spun up to farm an event, not to " +
					"rank real projects against each other - which is why the spread in the final split is narrower " +
					"than the spread in raw contributor counts. Criteria with too little data to be meaningful are " +
					"dropped and their weight shared across the rest, rather than counted as a zero.",
			},
			// Structural rules that live in code rather than config, because
			// they protect an ordering that a config edit must not be able to
			// invert. Published for the same reason as everything else.
			"structural": fiber.Map{
				"prior_completion_cap": hackathon.PriorCompletionCap,
				"prior_completion_cap_note": "The prior-completion weight compounds per completed issue but stops at this many, " +
					"so having won before can never outrank demonstrated capability for the issue you're applying to.",
			},
		})
	}
}
