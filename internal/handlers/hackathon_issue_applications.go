package handlers

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/ai"
	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// HackathonIssueApplicationsHandler is the contributor-facing half of
// AI-specs.md §4: applying to a GrainHack issue, listing your own
// applications, and voluntarily releasing an assignment.
type HackathonIssueApplicationsHandler struct {
	cfg config.Config
	db  *db.DB
	gh  *github.Client
	ai  *ai.Client
}

func NewHackathonIssueApplicationsHandler(cfg config.Config, d *db.DB) *HackathonIssueApplicationsHandler {
	return &HackathonIssueApplicationsHandler{
		cfg: cfg,
		db:  d,
		gh:  github.NewClient(),
		ai:  ai.NewClient(cfg.AnthropicAPIKey),
	}
}

type applyToHackathonIssueRequest struct {
	// Optional and explicitly untrusted (§4.4). Never weighted by the draw;
	// passed to Layer 2 only so it can flag injection attempts.
	ApplicationText string `json:"application_text"`
}

type hackathonIssueApplicationDTO struct {
	ID             uuid.UUID  `json:"id"`
	HackathonID    uuid.UUID  `json:"hackathon_id"`
	HackathonName  string     `json:"hackathon_name"`
	IssueID        uuid.UUID  `json:"hackathon_issue_id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	RepoFullName   string     `json:"repo_full_name"`
	IssueNumber    int        `json:"issue_number"`
	Status         string     `json:"status"`
	GateFailure    *string    `json:"gate_failure_reason"`
	Fit            *string    `json:"fit"`
	WindowClosesAt *time.Time `json:"application_window_closes_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Apply handles POST /hackathon-issues/:id/apply.
//
// The §4.1 hard gates run synchronously here so a rejected applicant sees
// the specific reason immediately ("Any failure rejects the application with
// the specific reason shown to the applicant"). Rejections are still
// persisted - an appeal needs to see why someone never entered a pool.
func (h *HackathonIssueApplicationsHandler) Apply() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		issueID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_id"})
		}
		var req applyToHackathonIssueRequest
		_ = c.BodyParser(&req)

		// The applicant's own token, not the app installation token: the
		// pre-event-activity gate searches their commit history, and search
		// is scoped to what the token can see. See ApplicantContext.SearchToken.
		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "github_account_required"})
		}
		login := linked.Login

		var ac hackathon.ApplicantContext
		var issueAuthor *string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT hi.hackathon_id, hi.id, hi.project_id, hi.issue_number, hi.org_login, p.github_full_name, gi.author_login
FROM hackathon_issues hi
JOIN projects p ON p.id = hi.project_id
LEFT JOIN github_issues gi ON gi.project_id = hi.project_id AND gi.number = hi.issue_number
WHERE hi.id = $1
`, issueID).Scan(&ac.HackathonID, &ac.IssueID, &ac.ProjectID, &ac.IssueNumber, &ac.OrgLogin, &ac.RepoFullName, &issueAuthor)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "issue_not_found"})
		}
		if err != nil {
			slog.Error("hackathon apply: load issue", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "apply_failed"})
		}
		ac.UserID = userID
		ac.GitHubLogin = login
		ac.SearchToken = linked.AccessToken
		if issueAuthor != nil {
			ac.IssueAuthorLogin = *issueAuthor
		}

		// A re-application after losing a draw is fine; a duplicate while
		// one is still open is not.
		var existingStatus string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT status FROM hackathon_issue_applications WHERE hackathon_issue_id = $1 AND user_id = $2
`, issueID, userID).Scan(&existingStatus)
		if err == nil && existingStatus == "applied" {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "already_applied"})
		}

		gate, err := hackathon.CheckGates(c.Context(), h.db.Pool, h.gh, h.installationTokenFor(c, ac.ProjectID), ac)
		if err != nil {
			slog.Error("hackathon apply: gates", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "apply_failed"})
		}

		status := "applied"
		var gateReason *string
		if !gate.Passed {
			status = "rejected_gate"
			r := gate.Reason
			gateReason = &r
		}

		var applicationID uuid.UUID
		if err := h.db.Pool.QueryRow(c.Context(), `
INSERT INTO hackathon_issue_applications
  (hackathon_id, hackathon_issue_id, user_id, github_login, status, gate_failure_reason, application_text)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))
ON CONFLICT (hackathon_issue_id, user_id) DO UPDATE
SET status = EXCLUDED.status,
    gate_failure_reason = EXCLUDED.gate_failure_reason,
    application_text = EXCLUDED.application_text,
    fit = NULL, difficulty_match = NULL, fit_assessed_at = NULL,
    updated_at = now()
RETURNING id
`, ac.HackathonID, issueID, userID, login, status, gateReason, strings.TrimSpace(req.ApplicationText)).Scan(&applicationID); err != nil {
			slog.Error("hackathon apply: insert", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "apply_failed"})
		}

		if !gate.Passed {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":  "gate_failed",
				"gate":   gate.Gate,
				"reason": gate.Reason,
			})
		}

		// Layer 2 (§4.3) - one call per applicant, at application time
		// rather than at the draw, so the window closing isn't gated on a
		// burst of model calls. A failure inside AssessFit already falls
		// back to "plausible", so this never blocks the application.
		if err := hackathon.AssessFit(c.Context(), h.db.Pool, h.ai, h.gh,
			h.installationTokenFor(c, ac.ProjectID), applicationID); err != nil {
			slog.Warn("hackathon apply: fit assessment", "application_id", applicationID, "error", err)
		}

		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": applicationID, "status": "applied"})
	}
}

// Mine handles GET /hackathon-issue-applications/me.
func (h *HackathonIssueApplicationsHandler) Mine() fiber.Handler {
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
SELECT a.id, a.hackathon_id, h.name, a.hackathon_issue_id, hi.project_id, p.github_full_name,
       hi.issue_number, a.status, a.gate_failure_reason, a.fit, hi.application_window_closes_at, a.created_at
FROM hackathon_issue_applications a
JOIN hackathons h ON h.id = a.hackathon_id
JOIN hackathon_issues hi ON hi.id = a.hackathon_issue_id
JOIN projects p ON p.id = hi.project_id
WHERE a.user_id = $1
ORDER BY a.created_at DESC
`, userID)
		if err != nil {
			slog.Error("hackathon applications mine", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []hackathonIssueApplicationDTO{}
		for rows.Next() {
			var d hackathonIssueApplicationDTO
			if err := rows.Scan(&d.ID, &d.HackathonID, &d.HackathonName, &d.IssueID, &d.ProjectID,
				&d.RepoFullName, &d.IssueNumber, &d.Status, &d.GateFailure, &d.Fit,
				&d.WindowClosesAt, &d.CreatedAt); err != nil {
				slog.Error("hackathon applications mine: scan", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, d)
		}
		return c.JSON(fiber.Map{"applications": out})
	}
}

type hackathonAssignmentDTO struct {
	ID             uuid.UUID  `json:"id"`
	HackathonID    uuid.UUID  `json:"hackathon_id"`
	HackathonName  string     `json:"hackathon_name"`
	IssueID        uuid.UUID  `json:"hackathon_issue_id"`
	RepoFullName   string     `json:"repo_full_name"`
	IssueNumber    int        `json:"issue_number"`
	GitHubLogin    string     `json:"github_login"`
	Status         string     `json:"status"`
	HoldsSlot      bool       `json:"holds_slot"`
	AssignedAt     time.Time  `json:"assigned_at"`
	StaleAt        *time.Time `json:"stale_at"`
	ReleaseReason  *string    `json:"release_reason"`
	AbandonRecord  bool       `json:"abandon_recorded"`
	QualifyingPR   *int       `json:"qualifying_pr_number"`
	AssociationRaw []byte     `json:"-"`
}

// MyAssignments handles GET /hackathon-assignments/me.
func (h *HackathonIssueApplicationsHandler) MyAssignments() fiber.Handler {
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
SELECT a.id, a.hackathon_id, h.name, a.hackathon_issue_id, p.github_full_name, a.issue_number,
       a.github_login, a.status, a.holds_slot, a.assigned_at, a.stale_at, a.release_reason,
       a.abandon_recorded, a.qualifying_pr_number
FROM hackathon_assignments a
JOIN hackathons h ON h.id = a.hackathon_id
JOIN projects p ON p.id = a.project_id
WHERE a.user_id = $1
ORDER BY a.assigned_at DESC
`, userID)
		if err != nil {
			slog.Error("hackathon assignments mine", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []hackathonAssignmentDTO{}
		for rows.Next() {
			var d hackathonAssignmentDTO
			if err := rows.Scan(&d.ID, &d.HackathonID, &d.HackathonName, &d.IssueID, &d.RepoFullName,
				&d.IssueNumber, &d.GitHubLogin, &d.Status, &d.HoldsSlot, &d.AssignedAt, &d.StaleAt,
				&d.ReleaseReason, &d.AbandonRecord, &d.QualifyingPR); err != nil {
				slog.Error("hackathon assignments mine: scan", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, d)
		}
		return c.JSON(fiber.Map{"assignments": out})
	}
}

// Release handles POST /hackathon-assignments/:id/release - §4.6's voluntary
// release. Inside voluntary_release_grace_hours it costs nothing; after it,
// an abandon is recorded. The response says which happened so the UI can
// warn before the user commits to it.
func (h *HackathonIssueApplicationsHandler) Release() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		assignmentID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_assignment_id"})
		}

		var hackathonID, projectID, issueID uuid.UUID
		var issueNumber int
		var login, fullName string
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT a.hackathon_id, a.project_id, a.hackathon_issue_id, a.issue_number, a.github_login, p.github_full_name
FROM hackathon_assignments a JOIN projects p ON p.id = a.project_id
WHERE a.id = $1 AND a.user_id = $2
`, assignmentID, userID).Scan(&hackathonID, &projectID, &issueID, &issueNumber, &login, &fullName); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "assignment_not_found"})
		}

		abandoned, err := hackathon.ReleaseVoluntary(c.Context(), h.db.Pool, hackathonID, userID, assignmentID)
		if errors.Is(err, hackathon.ErrNoActiveAssignment) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "assignment_not_active"})
		}
		if err != nil {
			slog.Error("hackathon release", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "release_failed"})
		}

		if tok := h.installationTokenFor(c, projectID); tok != "" {
			_ = h.gh.RemoveIssueAssignees(c.Context(), tok, fullName, issueNumber, []string{login})
		}
		// Straight back into the pool - an issue nobody is working on
		// helps nobody.
		if err := hackathon.ReopenWindow(c.Context(), h.db.Pool, hackathonID, issueID); err != nil {
			slog.Warn("hackathon release: reopen window", "error", err)
		}

		return c.JSON(fiber.Map{"ok": true, "abandon_recorded": abandoned})
	}
}

// installationTokenFor resolves a GitHub App installation token for a
// project, or "" when the app isn't configured. GitHub-backed gates degrade
// rather than fail when this is empty - see hackathon.CheckGates.
func (h *HackathonIssueApplicationsHandler) installationTokenFor(c *fiber.Ctx, projectID uuid.UUID) string {
	if strings.TrimSpace(h.cfg.GitHubAppID) == "" || strings.TrimSpace(h.cfg.GitHubAppPrivateKey) == "" {
		return ""
	}
	var installationID string
	if err := h.db.Pool.QueryRow(c.Context(),
		`SELECT COALESCE(github_app_installation_id, '') FROM projects WHERE id = $1`, projectID).Scan(&installationID); err != nil {
		return ""
	}
	if strings.TrimSpace(installationID) == "" {
		return ""
	}
	appClient, err := github.NewGitHubAppClient(h.cfg.GitHubAppID, h.cfg.GitHubAppPrivateKey)
	if err != nil {
		return ""
	}
	tok, err := appClient.GetInstallationToken(c.Context(), installationID)
	if err != nil {
		return ""
	}
	return tok
}
