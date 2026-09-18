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
	// All three published together, never net alone. Disclosure is the whole
	// justification for taking a fee - showing only the net pool would make it
	// a skim, and a reader could not tell the difference.
	SponsorTotalUSDC *string `json:"sponsor_total_usdc"`
	PlatformFeeUSDC  *string `json:"platform_fee_usdc"`
	// Net, i.e. what actually pays people. These are the same columns every
	// payout path divides.
	ContributorPrizePool *string `json:"contributor_prize_pool"`
	MaintainerPrizePool  *string `json:"maintainer_prize_pool"`
	NetPoolUSDC          *string `json:"net_pool_usdc"`
	PlatformFeeRatePct   *string `json:"platform_fee_rate_pct"`

	CreatedAt time.Time `json:"created_at"`
}

const hackathonSelectCols = `
id, name, phase, announced_at, application_period_start, application_period_end, issue_prep_start,
starts_at, ends_at, merge_grace_period_hours,
sponsor_total_usdc::text, platform_fee_usdc::text,
contributor_prize_pool::text, maintainer_prize_pool::text,
(COALESCE(contributor_prize_pool,0) + COALESCE(maintainer_prize_pool,0))::text,
platform_fee_rate_pct::text,
created_at
`

func scanHackathon(row interface{ Scan(...any) error }) (hackathonDTO, error) {
	var h hackathonDTO
	err := row.Scan(&h.ID, &h.Name, &h.Phase, &h.AnnouncedAt, &h.ApplicationPeriodStart, &h.ApplicationPeriodEnd,
		&h.IssuePrepStart, &h.StartsAt, &h.EndsAt, &h.MergeGracePeriodHours,
		&h.SponsorTotalUSDC, &h.PlatformFeeUSDC,
		&h.ContributorPrizePool, &h.MaintainerPrizePool, &h.NetPoolUSDC, &h.PlatformFeeRatePct,
		&h.CreatedAt)
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

// publicHackathonIssueDTO is the contributor-facing shape of one published
// issue: enough to decide whether to apply, and nothing that lets someone
// compare how contested it is against any other issue.
//
// Deliberately narrower than both admin's hackathonIssueDTO (ListForHackathon)
// and the owner-or-admin-gated ListForProject: no applicant_count or
// applicant_bucket (a list of buckets is a sortable comparison a single-issue
// view is not, and the draw's weighting exists specifically to make that
// comparison worthless - see the design note this endpoint was built from),
// no flagged_for_admin/flagged_reason/org_login/synced_at (moderation and
// sync bookkeeping with no contributor-facing purpose), and nothing from
// hackathon_issue_applications at all - not even a count of it.
type publicHackathonIssueDTO struct {
	ID                        uuid.UUID  `json:"id"`
	ProjectID                 uuid.UUID  `json:"project_id"`
	RepoFullName              string     `json:"repo_full_name"`
	IssueNumber               int        `json:"issue_number"`
	IssueTitle                string     `json:"issue_title"`
	DifficultyTier            string     `json:"difficulty_tier"`
	AcceptanceCriteria        string     `json:"acceptance_criteria"`
	Reserved                  bool       `json:"reserved"`
	ApplicationWindowOpensAt  *time.Time `json:"application_window_opens_at"`
	ApplicationWindowClosesAt *time.Time `json:"application_window_closes_at"`
	// Assigned is true once the issue's draw has produced an assignment that
	// is still held or was completed - so a page can stop saying "the draw
	// runs shortly" after it has run. Deliberately a bare boolean: never who
	// holds it. Naming the winner on a public page gives everyone who lost
	// the draw a worse experience than a status word, and nothing needs it.
	// A released assignment frees the issue again, so it does not count.
	Assigned bool `json:"assigned"`
}

// IssuesForHackathon handles GET /hackathons/:id/issues - every published
// issue across every project in this hackathon. Public in the same sense
// GetByID is: no RequireAuth, and a draft hackathon's issues do not exist
// publicly either, checked the same way (rather than trusting the id alone
// and letting a join produce rows for a hackathon nobody should be able to
// see yet).
//
// This is the endpoint GET /projects/:id/hackathon-issues could not be: that
// route is owner-or-admin gated per project, so a contributor who owns none
// of a hackathon's projects gets 403 from it for every one of them. Nothing
// before this endpoint let a contributor list an event's issues at all.
//
// Ordered by published_at ascending - publication order, which carries no
// information about how contested an issue is. Deliberately not ordered by
// anything that could (difficulty, acceptance-criteria length, an inferred
// popularity), since an ordering can leak the same signal a missing field
// was built to withhold.
func (h *HackathonPublicHandler) IssuesForHackathon() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		var exists bool
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT EXISTS(SELECT 1 FROM hackathons WHERE id = $1 AND phase != 'draft')
`, id).Scan(&exists); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "hackathon_lookup_failed"})
		}
		if !exists {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT hi.id, hi.project_id, p.github_full_name, hi.issue_number,
       COALESCE(gi.title, ''), COALESCE(hi.difficulty_tier, ''), COALESCE(hi.acceptance_criteria, ''),
       COALESCE(hi.reserved, false), hi.application_window_opens_at, hi.application_window_closes_at,
       EXISTS (
         SELECT 1 FROM hackathon_assignments a
         WHERE a.hackathon_issue_id = hi.id
           AND a.status IN ('active', 'pr_submitted', 'completed')
       )
FROM hackathon_issues hi
JOIN projects p ON p.id = hi.project_id
LEFT JOIN github_issues gi ON gi.project_id = hi.project_id AND gi.number = hi.issue_number
WHERE hi.hackathon_id = $1 AND hi.status = 'published'
ORDER BY hi.published_at ASC
`, id)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_list_failed"})
		}
		defer rows.Close()

		out := []publicHackathonIssueDTO{}
		for rows.Next() {
			var d publicHackathonIssueDTO
			if err := rows.Scan(&d.ID, &d.ProjectID, &d.RepoFullName, &d.IssueNumber,
				&d.IssueTitle, &d.DifficultyTier, &d.AcceptanceCriteria,
				&d.Reserved, &d.ApplicationWindowOpensAt, &d.ApplicationWindowClosesAt, &d.Assigned); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_scan_failed"})
			}
			out = append(out, d)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"issues": out})
	}
}
