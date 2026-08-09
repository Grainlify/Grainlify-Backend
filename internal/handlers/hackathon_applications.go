package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type HackathonApplicationsHandler struct {
	db *db.DB
}

func NewHackathonApplicationsHandler(d *db.DB) *HackathonApplicationsHandler {
	return &HackathonApplicationsHandler{db: d}
}

type applyToHackathonRequest struct {
	ProjectIDs         []uuid.UUID `json:"project_ids"`
	ShortDescription   string      `json:"short_description"`
	Goal               string      `json:"goal"`
	ExpectedIssueCount int         `json:"expected_issue_count"`
	MaintainerContact  string      `json:"maintainer_contact"`
}

type hackathonApplicationDTO struct {
	ID                 uuid.UUID  `json:"id"`
	HackathonID        uuid.UUID  `json:"hackathon_id"`
	HackathonName      string     `json:"hackathon_name"`
	ProjectID          uuid.UUID  `json:"project_id"`
	ProjectFullName    string     `json:"project_full_name"`
	ShortDescription   string     `json:"short_description"`
	Goal               string     `json:"goal"`
	ExpectedIssueCount int        `json:"expected_issue_count"`
	MaintainerContact  string     `json:"maintainer_contact"`
	Status             string     `json:"status"`
	ReviewReason       *string    `json:"review_reason"`
	ReviewedAt         *time.Time `json:"reviewed_at"`
	CreatedAt          time.Time  `json:"created_at"`
}

// Apply handles POST /hackathons/:id/applications. Fans out across
// project_ids into one hackathon_project_applications row each (AI-specs.md
// §2.1 - "an org applying with 3 repos" has no first-class org grouping
// anywhere in this codebase, so this is modeled as N independently-reviewable
// applications, not one). Every project must be owned by the caller,
// verified, and not deleted. Only accepted while the hackathon is in its
// application_period phase.
func (h *HackathonApplicationsHandler) Apply() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		var req applyToHackathonRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if len(req.ProjectIDs) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_ids_required"})
		}
		if req.ShortDescription == "" || req.Goal == "" || req.MaintainerContact == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_required_fields"})
		}

		var phase string
		if err := h.db.Pool.QueryRow(c.Context(), `SELECT phase FROM hackathons WHERE id = $1`, hackathonID).Scan(&phase); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "hackathon_not_found"})
		}
		if phase != "application_period" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "hackathon_not_accepting_applications"})
		}

		var createdIDs []uuid.UUID
		for _, projectID := range req.ProjectIDs {
			var ownerUserID uuid.UUID
			var status string
			var deletedAt *time.Time
			err := h.db.Pool.QueryRow(c.Context(), `
SELECT owner_user_id, status, deleted_at FROM projects WHERE id = $1
`, projectID).Scan(&ownerUserID, &status, &deletedAt)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_not_found", "project_id": projectID})
			}
			if ownerUserID != userID {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_project_owner", "project_id": projectID})
			}
			if status != "verified" || deletedAt != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "project_not_verified", "project_id": projectID})
			}

			var appID uuid.UUID
			err = h.db.Pool.QueryRow(c.Context(), `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
ON CONFLICT (hackathon_id, project_id) DO UPDATE SET
  short_description = EXCLUDED.short_description, goal = EXCLUDED.goal,
  expected_issue_count = EXCLUDED.expected_issue_count, maintainer_contact = EXCLUDED.maintainer_contact,
  status = 'pending', review_reason = NULL, reviewed_at = NULL, updated_at = now()
RETURNING id
`, hackathonID, projectID, userID, req.ShortDescription, req.Goal, req.ExpectedIssueCount, req.MaintainerContact).Scan(&appID)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "application_create_failed"})
			}
			createdIDs = append(createdIDs, appID)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"application_ids": createdIDs})
	}
}

// Mine handles GET /hackathon-applications/me - the caller's own
// applications, across every hackathon they've applied to.
func (h *HackathonApplicationsHandler) Mine() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT hpa.id, hpa.hackathon_id, h.name, hpa.project_id, p.github_full_name, hpa.short_description, hpa.goal,
       hpa.expected_issue_count, hpa.maintainer_contact, hpa.status, hpa.review_reason, hpa.reviewed_at, hpa.created_at
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
JOIN projects p ON p.id = hpa.project_id
WHERE hpa.applicant_user_id = $1
ORDER BY hpa.created_at DESC
`, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "applications_list_failed"})
		}
		defer rows.Close()

		out := []hackathonApplicationDTO{}
		for rows.Next() {
			var a hackathonApplicationDTO
			if err := rows.Scan(&a.ID, &a.HackathonID, &a.HackathonName, &a.ProjectID, &a.ProjectFullName, &a.ShortDescription,
				&a.Goal, &a.ExpectedIssueCount, &a.MaintainerContact, &a.Status, &a.ReviewReason, &a.ReviewedAt, &a.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "applications_scan_failed"})
			}
			out = append(out, a)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"applications": out})
	}
}
