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
	// Unenforced explains why Value is blank: the rule is real and published,
	// but its authority is a contract that does not exist yet, so there is no
	// number to show. The key and its description still appear - a reader
	// should know the rule exists and that we cannot yet quote it.
	Unenforced string `json:"unenforced,omitempty"`
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
			dto := ruleDTO{
				Key: key, Value: value, Type: def.Type, Section: def.Section,
				Description: def.Description, ValidRange: def.ValidRange, Active: def.Active,
			}
			// A setting whose authority is a deployed contract must not be
			// published as a number until it can be read back from that
			// contract. Showing 180 here while the escrow's own timelock is
			// whatever it was initialised with is the referral-window problem
			// again: a published rule the backend does not enforce.
			//
			// No escrow is deployed on any chain, so this suppresses
			// unconditionally today. When deployment exists, the value is
			// written here from the contract and this branch stops firing.
			if def.EnforcedOnChain {
				dto.Value = ""
				dto.Unenforced = "fixed in the escrow contract at deployment; no escrow is deployed yet, so there is no value to publish"
			}
			rules = append(rules, dto)
		}

		// §2.3: publish each chain's pool size from Phase 3, with issue and
		// applicant counts, so a contributor choosing between chains does so
		// with the same information we have. Separate pools mean rates differ
		// between chains, and that is only a problem if it is a surprise.
		chainPools := []fiber.Map{}
		if hackathonID != nil {
			rows, cerr := h.db.Pool.Query(c.Context(), `
SELECT p.chain_id,
       (p.contributor_pool / (10 ^ p.asset_decimals))::float8,
       (p.maintainer_pool  / (10 ^ p.asset_decimals))::float8,
       p.state,
       (SELECT count(*) FROM hackathon_issues hi
          WHERE hi.hackathon_id = p.hackathon_id AND hi.chain_id = p.chain_id
            AND hi.status = 'published')::int
FROM hackathon_chain_pools p
WHERE p.hackathon_id = $1
ORDER BY p.chain_id`, *hackathonID)
			if cerr != nil {
				slog.Warn("rules: chain pools", "error", cerr)
			} else {
				defer rows.Close()
				for rows.Next() {
					var chainID, state string
					var contributor, maintainer float64
					var issues int
					if err := rows.Scan(&chainID, &contributor, &maintainer, &state, &issues); err != nil {
						continue
					}
					chainPools = append(chainPools, fiber.Map{
						"chain_id":         chainID,
						"contributor_pool": contributor,
						"maintainer_pool":  maintainer,
						"escrow_state":     state,
						"published_issues": issues,
						// Bucketed, exactly as applicant counts are (§2.3), so
						// nobody can time an application against an exact number.
						"issue_volume": func() string {
							if issues >= 20 {
								return "many"
							}
							return "few"
						}(),
					})
				}
			}
		}

		// The fee, published as three separate numbers rather than a net-only
		// figure. A rules page that shows only what is left after a deduction
		// is not disclosing the deduction - and disclosure is the entire
		// justification for taking one.
		var feeBreakdown fiber.Map
		if hackathonID != nil {
			var total, fee, contributor, maintainer, ratePct *float64
			if err := h.db.Pool.QueryRow(c.Context(), `
SELECT sponsor_total_usdc::float8, platform_fee_usdc::float8,
       contributor_prize_pool::float8, maintainer_prize_pool::float8,
       platform_fee_rate_pct::float8
FROM hackathons WHERE id = $1
`, *hackathonID).Scan(&total, &fee, &contributor, &maintainer, &ratePct); err != nil {
				slog.Warn("rules: fee breakdown", "error", err)
			} else if total != nil {
				net := 0.0
				if contributor != nil {
					net += *contributor
				}
				if maintainer != nil {
					net += *maintainer
				}
				feeBreakdown = fiber.Map{
					"sponsor_total_usdc":    *total,
					"platform_fee_usdc":     fee,
					"platform_fee_rate_pct": ratePct,
					"net_pool_usdc":         net,
					"contributor_pool_usdc": contributor,
					"maintainer_pool_usdc":  maintainer,
				}
			}
		}

		return c.JSON(fiber.Map{
			"fee_breakdown":  feeBreakdown,
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
			"chain_pools": chainPools,
			"section_notes": fiber.Map{
				"Chains": "Each chain in this event has its own prize pool, funded by its own sponsor. " +
					"That money pays that chain's contributors and nobody else's, and it never moves between " +
					"chains. One consequence is worth stating plainly: identical work can pay differently on " +
					"different chains. If one chain's pool draws far more accepted pull requests than another's, " +
					"each one is worth less there. That is what separate pools mean, and the pool sizes and " +
					"issue counts are published here from the moment the event goes live so you can choose with " +
					"the same information we have. " +
					"The diminishing-returns curve is also counted per chain, so working across two chains " +
					"restarts it on each. That is a known property of separate pools rather than a loophole - " +
					"the limits on how many issues you can hold at once still apply across the whole event, " +
					"however you spread them.",
				"Judging and payout": "Each additional pull request you get accepted is worth progressively less. " +
					"Your first accepted PR counts in full, your second a little less, and so on down the published " +
					"curve, with the last value repeating after that. Position is by merge time and counts only PRs " +
					"that were accepted - one that was rejected does not use up a place. " +
					"You cannot work out a specific PR's multiplier while the event is running, because its position " +
					"depends on how many you end up getting accepted in total, and that is not known until the event " +
					"closes. The units this removes are not kept back: they raise the value of everyone else's share, " +
					"which is the point of doing it. The payout floor is applied afterwards, so a PR whose share falls " +
					"below the floor is handled by the published floor rule rather than being paid a token amount.",
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
