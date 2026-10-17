package handlers

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

type AdminHackathonsHandler struct {
	db *db.DB
}

func NewAdminHackathonsHandler(d *db.DB) *AdminHackathonsHandler {
	return &AdminHackathonsHandler{db: d}
}

func adminID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

type createHackathonRequest struct {
	Name string `json:"name"`
}

// Create handles POST /admin/hackathons - starts a hackathon in 'draft'
// (AI-specs.md §1 Phase 0: "Admin creates the hackathon... Not visible
// publicly."). Every other field is set via Update() afterward.
func (h *AdminHackathonsHandler) Create() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		var req createHackathonRequest
		if err := c.BodyParser(&req); err != nil || req.Name == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name_required"})
		}

		var id uuid.UUID
		err := h.db.Pool.QueryRow(c.Context(), `
INSERT INTO hackathons (name, created_by) VALUES ($1, $2) RETURNING id
`, req.Name, actorID).Scan(&id)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_create_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"id": id})
	}
}

// List handles GET /admin/hackathons - every hackathon regardless of phase.
func (h *AdminHackathonsHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		rows, err := h.db.Pool.Query(c.Context(), `
SELECT `+hackathonSelectCols+`
FROM hackathons ORDER BY created_at DESC LIMIT 200
`)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathons_list_failed"})
		}
		defer rows.Close()

		out := []hackathonDTO{}
		for rows.Next() {
			hd, err := scanHackathon(rows)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathons_scan_failed"})
			}
			out = append(out, hd)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"hackathons": out})
	}
}

// GetByID handles GET /admin/hackathons/:id - includes what's blocking the
// next phase transition, so "the admin dashboard shows the current phase
// and what is blocking the next one" (AI-specs.md §1) has real data to show.
func (h *AdminHackathonsHandler) GetByID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		row := h.db.Pool.QueryRow(c.Context(), `SELECT `+hackathonSelectCols+` FROM hackathons WHERE id = $1`, id)
		hd, err := scanHackathon(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_fetch_failed"})
		}

		blocking, nextPhase, err := hackathon.Readiness(c.Context(), h.db.Pool, id)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "readiness_check_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"hackathon":        hd,
			"next_phase":       nextPhase,
			"blocking_reasons": blocking,
		})
	}
}

type updateHackathonRequest struct {
	Name                   *string    `json:"name"`
	AnnouncedAt            *time.Time `json:"announced_at"`
	ApplicationPeriodStart *time.Time `json:"application_period_start"`
	ApplicationPeriodEnd   *time.Time `json:"application_period_end"`
	IssuePrepStart         *time.Time `json:"issue_prep_start"`
	StartsAt               *time.Time `json:"starts_at"`
	EndsAt                 *time.Time `json:"ends_at"`
	MergeGracePeriodHours  *int       `json:"merge_grace_period_hours"`

	// What the sponsor is putting in. The pools are DERIVED from this - the
	// platform fee comes off first, and the remainder splits - so an admin
	// never types a pool figure directly. Letting them set a pool would let
	// the recorded fee and the recorded pools disagree, which is exactly the
	// ambiguity the stored breakdown exists to remove.
	SponsorTotalUSDC *float64 `json:"sponsor_total_usdc"`
}

// Update handles PUT /admin/hackathons/:id. Every field is optional - only
// provided fields are changed (COALESCE against the existing row), matching
// how a form saves one field at a time as an admin fills in requirements.
// Writes a config_audit row for every changed field to maintain auditability.
func (h *AdminHackathonsHandler) Update() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		var req updateHackathonRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		// This endpoint edits the prize pools, so an unattributed change here
		// is an unattributed change to money.
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		// The fee is applied here, at record time, and nowhere else.
		//
		// The obvious place would be where the pools are committed to escrow -
		// but nothing writes a chain pool row and FundAllEscrows has no
		// caller, so a fee applied there would never execute. Every payout
		// that can actually happen reads the columns written below.
		var (
			sponsorTotal *float64
			feeUSDC      *float64
			feeRatePct   *float64
			maintPct     *float64
			contributor  *float64
			maintainer   *float64
		)
		if req.SponsorTotalUSDC != nil {
			cfg, cfgErr := hackathon.EffectiveValues(c.Context(), h.db.Pool, &id)
			if cfgErr != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "config_load_failed"})
			}
			split, splitErr := hackathon.SplitSponsorTotal(*req.SponsorTotalUSDC, cfg)
			if splitErr != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_sponsor_total"})
			}
			// Refuse rather than store a breakdown that does not add up. A fee
			// that silently fails to reconcile is indistinguishable from a
			// skim, and the database CHECK would reject it anyway - failing
			// here gives the caller a reason instead of a constraint error.
			if !split.Reconciles() {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "fee_split_does_not_reconcile"})
			}
			sponsorTotal = &split.SponsorTotal
			feeUSDC = &split.PlatformFee
			feeRatePct = &split.FeeRatePct
			maintPct = &split.MaintainerSharePct
			contributor = &split.ContributorPool
			maintainer = &split.MaintainerPool
		}

		tx, err := h.db.Pool.Begin(c.Context())
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "db_tx_start_failed"})
		}
		defer func() { _ = tx.Rollback(c.Context()) }()

		var (
			oldName            string
			oldAnnouncedAt     *time.Time
			oldAppPeriodStart  *time.Time
			oldAppPeriodEnd    *time.Time
			oldIssuePrepStart  *time.Time
			oldStartsAt        *time.Time
			oldEndsAt          *time.Time
			oldGraceHours      int
			oldSponsorTotal    *string
			oldPlatformFee     *string
			oldPlatformFeeRate *string
			oldMaintainerShare *string
			oldContributorPool *string
			oldMaintainerPool  *string
		)

		err = tx.QueryRow(c.Context(), `
SELECT name, announced_at, application_period_start, application_period_end, issue_prep_start,
       starts_at, ends_at, merge_grace_period_hours,
       sponsor_total_usdc::text, platform_fee_usdc::text, platform_fee_rate_pct::text,
       maintainer_share_pct::text, contributor_prize_pool::text, maintainer_prize_pool::text
FROM hackathons WHERE id = $1 FOR UPDATE
`, id).Scan(
			&oldName, &oldAnnouncedAt, &oldAppPeriodStart, &oldAppPeriodEnd, &oldIssuePrepStart,
			&oldStartsAt, &oldEndsAt, &oldGraceHours,
			&oldSponsorTotal, &oldPlatformFee, &oldPlatformFeeRate,
			&oldMaintainerShare, &oldContributorPool, &oldMaintainerPool,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_fetch_failed"})
		}

		_, err = tx.Exec(c.Context(), `
UPDATE hackathons SET
  name = COALESCE($2, name),
  announced_at = COALESCE($3, announced_at),
  application_period_start = COALESCE($4, application_period_start),
  application_period_end = COALESCE($5, application_period_end),
  issue_prep_start = COALESCE($6, issue_prep_start),
  starts_at = COALESCE($7, starts_at),
  ends_at = COALESCE($8, ends_at),
  merge_grace_period_hours = COALESCE($9, merge_grace_period_hours),
  sponsor_total_usdc = COALESCE($10, sponsor_total_usdc),
  platform_fee_usdc = COALESCE($11, platform_fee_usdc),
  platform_fee_rate_pct = COALESCE($12, platform_fee_rate_pct),
  maintainer_share_pct = COALESCE($13, maintainer_share_pct),
  contributor_prize_pool = COALESCE($14, contributor_prize_pool),
  maintainer_prize_pool = COALESCE($15, maintainer_prize_pool),
  updated_by = $16,
  updated_at = now()
WHERE id = $1
`, id, req.Name, req.AnnouncedAt, req.ApplicationPeriodStart, req.ApplicationPeriodEnd, req.IssuePrepStart,
			req.StartsAt, req.EndsAt, req.MergeGracePeriodHours,
			sponsorTotal, feeUSDC, feeRatePct, maintPct, contributor, maintainer,
			actorID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_update_failed"})
		}

		auditDiff := func(key string, oldVal *string, newVal *string) error {
			if newVal == nil {
				return nil
			}
			if oldVal != nil && *oldVal == *newVal {
				return nil
			}
			_, err := tx.Exec(c.Context(), `
INSERT INTO config_audit (hackathon_id, key, old_value, new_value, actor_user_id)
VALUES ($1, $2, $3, $4, $5)
`, id, key, oldVal, newVal, actorID)
			return err
		}

		auditTimeDiff := func(key string, oldTime *time.Time, newTime *time.Time) error {
			if newTime == nil {
				return nil
			}
			var oldStr *string
			if oldTime != nil {
				s := oldTime.UTC().Format(time.RFC3339)
				oldStr = &s
			}
			newStr := newTime.UTC().Format(time.RFC3339)
			return auditDiff(key, oldStr, &newStr)
		}

		if req.Name != nil {
			if err := auditDiff("name", &oldName, req.Name); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.AnnouncedAt != nil {
			if err := auditTimeDiff("announced_at", oldAnnouncedAt, req.AnnouncedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.ApplicationPeriodStart != nil {
			if err := auditTimeDiff("application_period_start", oldAppPeriodStart, req.ApplicationPeriodStart); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.ApplicationPeriodEnd != nil {
			if err := auditTimeDiff("application_period_end", oldAppPeriodEnd, req.ApplicationPeriodEnd); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.IssuePrepStart != nil {
			if err := auditTimeDiff("issue_prep_start", oldIssuePrepStart, req.IssuePrepStart); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.StartsAt != nil {
			if err := auditTimeDiff("starts_at", oldStartsAt, req.StartsAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.EndsAt != nil {
			if err := auditTimeDiff("ends_at", oldEndsAt, req.EndsAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.MergeGracePeriodHours != nil {
			oldGraceStr := fmt.Sprintf("%d", oldGraceHours)
			newGraceStr := fmt.Sprintf("%d", *req.MergeGracePeriodHours)
			if err := auditDiff("merge_grace_period_hours", &oldGraceStr, &newGraceStr); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}
		if req.SponsorTotalUSDC != nil {
			formatFloat := func(f *float64) *string {
				if f == nil {
					return nil
				}
				s := fmt.Sprintf("%.6f", *f)
				s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
				return &s
			}
			if err := auditDiff("sponsor_total_usdc", oldSponsorTotal, formatFloat(sponsorTotal)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
			if err := auditDiff("platform_fee_usdc", oldPlatformFee, formatFloat(feeUSDC)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
			if err := auditDiff("platform_fee_rate_pct", oldPlatformFeeRate, formatFloat(feeRatePct)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
			if err := auditDiff("maintainer_share_pct", oldMaintainerShare, formatFloat(maintPct)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
			if err := auditDiff("contributor_prize_pool", oldContributorPool, formatFloat(contributor)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
			if err := auditDiff("maintainer_prize_pool", oldMaintainerPool, formatFloat(maintainer)); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_write_failed"})
			}
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "tx_commit_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type transitionHackathonRequest struct {
	ToPhase string `json:"to_phase"`
}

// Transition handles POST /admin/hackathons/:id/transition. Sequential-only
// (internal/hackathon.Transition) - an explicit admin action per phase, not
// a cron job (AI-specs.md §1).
func (h *AdminHackathonsHandler) Transition() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		var req transitionHackathonRequest
		if err := c.BodyParser(&req); err != nil || req.ToPhase == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "to_phase_required"})
		}

		if err := hackathon.Transition(c.Context(), h.db.Pool, id, req.ToPhase, actorID); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "transition_failed", "message": err.Error()})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
