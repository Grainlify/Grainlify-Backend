package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type HackathonPublicHandler struct {
	db *db.DB
}

func NewHackathonPublicHandler(d *db.DB) *HackathonPublicHandler {
	return &HackathonPublicHandler{db: d}
}

type hackathonDTO struct {
	ID                     uuid.UUID  `json:"id"`
	Name                   string     `json:"name"`
	Phase                  string     `json:"phase"`
	AnnouncedAt            *time.Time `json:"announced_at"`
	ApplicationPeriodStart *time.Time `json:"application_period_start"`
	ApplicationPeriodEnd   *time.Time `json:"application_period_end"`
	IssuePrepStart         *time.Time `json:"issue_prep_start"`
	StartsAt               *time.Time `json:"starts_at"`
	EndsAt                 *time.Time `json:"ends_at"`
	MergeGracePeriodHours  int        `json:"merge_grace_period_hours"`
	ContributorPrizePool   *string    `json:"contributor_prize_pool"`
	MaintainerPrizePool    *string    `json:"maintainer_prize_pool"`
	CreatedAt              time.Time  `json:"created_at"`
}

const hackathonSelectCols = `
id, name, phase, announced_at, application_period_start, application_period_end, issue_prep_start,
starts_at, ends_at, merge_grace_period_hours, contributor_prize_pool::text, maintainer_prize_pool::text, created_at
`

func scanHackathon(row interface{ Scan(...any) error }) (hackathonDTO, error) {
	var h hackathonDTO
	err := row.Scan(&h.ID, &h.Name, &h.Phase, &h.AnnouncedAt, &h.ApplicationPeriodStart, &h.ApplicationPeriodEnd,
		&h.IssuePrepStart, &h.StartsAt, &h.EndsAt, &h.MergeGracePeriodHours, &h.ContributorPrizePool, &h.MaintainerPrizePool, &h.CreatedAt)
	return h, err
}

// List handles GET /hackathons - published (non-draft) hackathons only.
func (h *HackathonPublicHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		rows, err := h.db.Pool.Query(c.Context(), `
SELECT `+hackathonSelectCols+`
FROM hackathons WHERE phase != 'draft' ORDER BY created_at DESC LIMIT 100
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

// GetByID handles GET /hackathons/:id - 404s for a draft hackathon the same
// as a nonexistent one, since draft is "not visible publicly" (AI-specs.md §1).
func (h *HackathonPublicHandler) GetByID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		row := h.db.Pool.QueryRow(c.Context(), `
SELECT `+hackathonSelectCols+`
FROM hackathons WHERE id = $1 AND phase != 'draft'
`, id)
		hd, err := scanHackathon(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_fetch_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(hd)
	}
}
