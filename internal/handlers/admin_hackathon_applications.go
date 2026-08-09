package handlers

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type AdminHackathonApplicationsHandler struct {
	cfg    config.Config
	db     *db.DB
	gh     *github.Client
	notify *notifications.Service
}

func NewAdminHackathonApplicationsHandler(cfg config.Config, d *db.DB, notify *notifications.Service) *AdminHackathonApplicationsHandler {
	return &AdminHackathonApplicationsHandler{cfg: cfg, db: d, gh: github.NewClient(), notify: notify}
}

// signalsStaleAfter bounds how long a cached signals computation is trusted
// before Signals() recomputes it - GitHub state (commit activity, review
// latency) drifts slowly, so this favors avoiding repeated API calls over
// perfect freshness.
const signalsStaleAfter = time.Hour

// ListAdmin handles GET /admin/hackathons/:id/applications?status=pending.
// Deliberately does not eagerly compute signals for the whole page - see
// Signals() below.
func (h *AdminHackathonApplicationsHandler) ListAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		status := c.Query("status", "pending")

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT hpa.id, hpa.hackathon_id, h.name, hpa.project_id, p.github_full_name, hpa.short_description, hpa.goal,
       hpa.expected_issue_count, hpa.maintainer_contact, hpa.status, hpa.review_reason, hpa.reviewed_at, hpa.created_at
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
JOIN projects p ON p.id = hpa.project_id
WHERE hpa.hackathon_id = $1 AND hpa.status = $2
ORDER BY hpa.created_at ASC
LIMIT 200
`, hackathonID, status)
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

// Signals handles GET /admin/hackathons/applications/:appId/signals - the
// AI-specs.md §2.1 auto-collected signals, computed lazily on first review
// (not eagerly for the whole queue, since each computation is several
// GitHub API calls) and cached; recomputed if stale or ?refresh=true.
func (h *AdminHackathonApplicationsHandler) Signals() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		appID, err := uuid.Parse(c.Params("appId"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_application_id"})
		}

		var cached []byte
		var computedAt *time.Time
		err = h.db.Pool.QueryRow(c.Context(), `SELECT signals, signals_computed_at FROM hackathon_project_applications WHERE id = $1`, appID).Scan(&cached, &computedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "application_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "signals_lookup_failed"})
		}

		fresh := computedAt != nil && time.Since(*computedAt) < signalsStaleAfter
		if fresh && c.Query("refresh") != "true" {
			var out hackathon.Signals
			if err := json.Unmarshal(cached, &out); err == nil {
				return c.Status(fiber.StatusOK).JSON(out)
			}
		}

		signals, err := hackathon.Compute(c.Context(), h.cfg, h.db.Pool, h.gh, appID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "signals_compute_failed", "message": err.Error()})
		}
		encoded, _ := json.Marshal(signals)
		_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE hackathon_project_applications SET signals = $1, signals_computed_at = now() WHERE id = $2
`, encoded, appID)

		return c.Status(fiber.StatusOK).JSON(signals)
	}
}

// Accept handles POST /admin/hackathons/applications/:appId/accept.
func (h *AdminHackathonApplicationsHandler) Accept() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		appID, err := uuid.Parse(c.Params("appId"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_application_id"})
		}

		var applicantID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE hackathon_project_applications
SET status = 'accepted', reviewer_id = $1, review_reason = NULL, reviewed_at = now(), updated_at = now()
WHERE id = $2 AND status IN ('pending', 'more_info_requested')
RETURNING applicant_user_id
`, actorID, appID).Scan(&applicantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "application_not_found_or_not_pending"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "accept_failed"})
		}

		if h.notify != nil {
			h.notify.Notify(c.Context(), applicantID, notifications.TypeGrainHackApplicationAccepted,
				"GrainHack application accepted",
				"Your project's GrainHack application was accepted. You can now label issues to enter them into the event.",
				"",
			)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type reviewApplicationRequest struct {
	Reason string `json:"reason"`
}

// Reject handles POST /admin/hackathons/applications/:appId/reject. Unlike
// the cloned SocialFollowReview/RedemptionsReview template, reason is
// required here (AI-specs.md §2.1: "Rejection requires a reason, which is
// shown to the applicant").
func (h *AdminHackathonApplicationsHandler) Reject() fiber.Handler {
	return h.review("rejected")
}

// RequestMoreInfo handles POST /admin/hackathons/applications/:appId/request-more-info.
// Same reason-required contract as Reject - the applicant needs to know
// what to add before resubmitting.
func (h *AdminHackathonApplicationsHandler) RequestMoreInfo() fiber.Handler {
	return h.review("more_info_requested")
}

func (h *AdminHackathonApplicationsHandler) review(newStatus string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		appID, err := uuid.Parse(c.Params("appId"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_application_id"})
		}
		var req reviewApplicationRequest
		_ = c.BodyParser(&req)
		if req.Reason == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "reason_required"})
		}

		var applicantID uuid.UUID
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE hackathon_project_applications
SET status = $1, reviewer_id = $2, review_reason = $3, reviewed_at = now(), updated_at = now()
WHERE id = $4 AND status IN ('pending', 'more_info_requested')
RETURNING applicant_user_id
`, newStatus, actorID, req.Reason, appID).Scan(&applicantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "application_not_found_or_not_pending"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "review_failed"})
		}

		if h.notify != nil {
			title, body := "GrainHack application update", req.Reason
			if newStatus == "rejected" {
				title = "GrainHack application rejected"
			} else {
				title = "GrainHack application needs more info"
			}
			h.notify.Notify(c.Context(), applicantID, notifications.TypeGrainHackApplicationReviewed, title, body, "")
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
