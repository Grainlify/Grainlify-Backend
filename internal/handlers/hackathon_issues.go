package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

type HackathonIssuesHandler struct {
	db *db.DB
}

func NewHackathonIssuesHandler(d *db.DB) *HackathonIssuesHandler {
	return &HackathonIssuesHandler{db: d}
}

type hackathonIssueDTO struct {
	ID                 uuid.UUID  `json:"id"`
	HackathonID        uuid.UUID  `json:"hackathon_id"`
	HackathonName      string     `json:"hackathon_name"`
	ProjectID          uuid.UUID  `json:"project_id"`
	IssueNumber        int        `json:"issue_number"`
	OrgLogin           string     `json:"org_login"`
	Status             string     `json:"status"`
	AcceptanceCriteria string     `json:"acceptance_criteria"`
	DifficultyTier     string     `json:"difficulty_tier"`
	PrimaryLanguage    string     `json:"primary_language"`
	FlaggedForAdmin    bool       `json:"flagged_for_admin"`
	FlaggedReason      *string    `json:"flagged_reason"`
	SyncedAt           time.Time  `json:"synced_at"`
	PublishedAt        *time.Time `json:"published_at"`
}

// ListForHackathon handles GET /admin/hackathons/:id/issues?status=&flagged=.
func (h *HackathonIssuesHandler) ListForHackathon() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		query := `
SELECT hi.id, hi.hackathon_id, h.name, hi.project_id, hi.issue_number, hi.org_login, hi.status,
       COALESCE(hi.acceptance_criteria, ''), COALESCE(hi.difficulty_tier, ''), COALESCE(hi.primary_language, ''),
       hi.flagged_for_admin, hi.flagged_reason, hi.synced_at, hi.published_at
FROM hackathon_issues hi
JOIN hackathons h ON h.id = hi.hackathon_id
WHERE hi.hackathon_id = $1
`
		args := []any{hackathonID}
		if status := c.Query("status"); status != "" {
			args = append(args, status)
			query += ` AND hi.status = $2`
		}
		if c.Query("flagged") == "true" {
			query += ` AND hi.flagged_for_admin = true`
		}
		query += ` ORDER BY hi.synced_at DESC LIMIT 500`

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_list_failed"})
		}
		defer rows.Close()

		out := []hackathonIssueDTO{}
		for rows.Next() {
			d, err := scanHackathonIssue(rows)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_scan_failed"})
			}
			out = append(out, d)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"issues": out})
	}
}

func scanHackathonIssue(row interface{ Scan(...any) error }) (hackathonIssueDTO, error) {
	var d hackathonIssueDTO
	err := row.Scan(&d.ID, &d.HackathonID, &d.HackathonName, &d.ProjectID, &d.IssueNumber, &d.OrgLogin, &d.Status,
		&d.AcceptanceCriteria, &d.DifficultyTier, &d.PrimaryLanguage, &d.FlaggedForAdmin, &d.FlaggedReason, &d.SyncedAt, &d.PublishedAt)
	return d, err
}

// canManageProject mirrors IssueDetailPage.tsx's canManage gate server-side:
// the project owner, or a platform admin.
func (h *HackathonIssuesHandler) canManageProject(c *fiber.Ctx, projectID uuid.UUID) (bool, error) {
	userIDStr, _ := c.Locals(auth.LocalUserID).(string)
	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		return false, nil
	}
	role, _ := c.Locals(auth.LocalRole).(string)
	if role == "admin" {
		return true, nil
	}
	var ownerID uuid.UUID
	if err := h.db.Pool.QueryRow(c.Context(), `SELECT owner_user_id FROM projects WHERE id = $1`, projectID).Scan(&ownerID); err != nil {
		return false, err
	}
	return ownerID == userID, nil
}

// ListForProject handles GET /projects/:id/hackathon-issues - every
// hackathon_issues row for this project, owner-or-admin gated (maintainer-
// facing, not admin-only: a maintainer need not be a platform admin).
func (h *HackathonIssuesHandler) ListForProject() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		canManage, err := h.canManageProject(c, projectID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authorization_check_failed"})
		}
		if !canManage {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_authorized"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT hi.id, hi.hackathon_id, h.name, hi.project_id, hi.issue_number, hi.org_login, hi.status,
       COALESCE(hi.acceptance_criteria, ''), COALESCE(hi.difficulty_tier, ''), COALESCE(hi.primary_language, ''),
       hi.flagged_for_admin, hi.flagged_reason, hi.synced_at, hi.published_at
FROM hackathon_issues hi
JOIN hackathons h ON h.id = hi.hackathon_id
WHERE hi.project_id = $1 AND hi.status != 'removed'
ORDER BY hi.issue_number ASC
`, projectID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_list_failed"})
		}
		defer rows.Close()

		out := []hackathonIssueDTO{}
		for rows.Next() {
			d, err := scanHackathonIssue(rows)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issues_scan_failed"})
			}
			out = append(out, d)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"issues": out})
	}
}

// Get handles GET /projects/:id/hackathon-issues/:number - 404s (not 403)
// when there's simply no hackathon_issues row for this issue, so the
// frontend can tell "not part of a hackathon" apart from a real error and
// render nothing rather than an error state.
func (h *HackathonIssuesHandler) Get() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}
		canManage, err := h.canManageProject(c, projectID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authorization_check_failed"})
		}
		if !canManage {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_authorized"})
		}

		row := h.db.Pool.QueryRow(c.Context(), `
SELECT hi.id, hi.hackathon_id, h.name, hi.project_id, hi.issue_number, hi.org_login, hi.status,
       COALESCE(hi.acceptance_criteria, ''), COALESCE(hi.difficulty_tier, ''), COALESCE(hi.primary_language, ''),
       hi.flagged_for_admin, hi.flagged_reason, hi.synced_at, hi.published_at
FROM hackathon_issues hi
JOIN hackathons h ON h.id = hi.hackathon_id
WHERE hi.project_id = $1 AND hi.issue_number = $2 AND hi.status != 'removed'
`, projectID, issueNumber)
		d, err := scanHackathonIssue(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_a_hackathon_issue"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issue_fetch_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(d)
	}
}

type updateHackathonIssueFieldsRequest struct {
	AcceptanceCriteria *string `json:"acceptance_criteria"`
	DifficultyTier     *string `json:"difficulty_tier"`
	PrimaryLanguage    *string `json:"primary_language"`
}

var validDifficultyTiers = map[string]bool{"easy": true, "standard": true, "advanced": true}

// UpdateFields handles PUT /projects/:id/hackathon-issues/:number - the
// maintainer-facing "add acceptance criteria and difficulty tier" action
// (AI-specs.md §2.2). Setting primary_language here marks it as no longer
// auto-detected, so a later re-sync never overwrites the manual choice.
func (h *HackathonIssuesHandler) UpdateFields() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		projectID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_project_id"})
		}
		issueNumber, err := c.ParamsInt("number")
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_number"})
		}
		canManage, err := h.canManageProject(c, projectID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "authorization_check_failed"})
		}
		if !canManage {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_authorized"})
		}

		var req updateHackathonIssueFieldsRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if req.DifficultyTier != nil && *req.DifficultyTier != "" && !validDifficultyTiers[*req.DifficultyTier] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_difficulty_tier"})
		}

		var issueID uuid.UUID
		var hackathonID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE hackathon_issues SET
  acceptance_criteria = COALESCE($3, acceptance_criteria),
  difficulty_tier = COALESCE(NULLIF($4, ''), difficulty_tier),
  primary_language = COALESCE(NULLIF($5, ''), primary_language),
  primary_language_auto_detected = CASE WHEN $5 IS NOT NULL AND $5 != '' THEN false ELSE primary_language_auto_detected END,
  updated_at = now()
WHERE project_id = $1 AND issue_number = $2 AND status != 'removed'
RETURNING id, hackathon_id
`, projectID, issueNumber, req.AcceptanceCriteria, req.DifficultyTier, req.PrimaryLanguage).Scan(&issueID, &hackathonID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not_a_hackathon_issue"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "update_failed"})
		}

		if err := hackathon.MaybePublish(c.Context(), h.db.Pool, hackathonID, issueID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "publish_check_failed"})
		}

		row := h.db.Pool.QueryRow(c.Context(), `
SELECT hi.id, hi.hackathon_id, h.name, hi.project_id, hi.issue_number, hi.org_login, hi.status,
       COALESCE(hi.acceptance_criteria, ''), COALESCE(hi.difficulty_tier, ''), COALESCE(hi.primary_language, ''),
       hi.flagged_for_admin, hi.flagged_reason, hi.synced_at, hi.published_at
FROM hackathon_issues hi JOIN hackathons h ON h.id = hi.hackathon_id WHERE hi.id = $1
`, issueID)
		d, err := scanHackathonIssue(row)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "issue_fetch_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(d)
	}
}
