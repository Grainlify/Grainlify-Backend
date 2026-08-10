package handlers

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// AdminHackathonDrawsHandler exposes the draw machinery to admins: the
// simulate action (AI-specs.md §4.5 sanity-checking) and the stored draw
// history that makes an appeal answerable.
type AdminHackathonDrawsHandler struct {
	db *db.DB
}

func NewAdminHackathonDrawsHandler(d *db.DB) *AdminHackathonDrawsHandler {
	return &AdminHackathonDrawsHandler{db: d}
}

type simulateDrawRequest struct {
	// Optional. Reusing a previous draw's seed replays it exactly, which is
	// how an appeal gets re-examined; omitting it explores a fresh roll.
	Seed int64 `json:"seed"`
}

// Simulate handles POST /admin/hackathon-issues/:id/simulate-draw.
//
// Unlike judging, assignment cannot run in shadow mode - it either assigns
// or it doesn't. This runs the full §4.5 pipeline against the real applicant
// pool and returns the ticket breakdown and the winner it *would* have
// picked, without writing an assignment or consuming a slot. Combined with
// the stored seed, that's what lets weights be checked against a real pool
// before the first event decides anything for real.
func (h *AdminHackathonDrawsHandler) Simulate() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		issueID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_issue_id"})
		}
		var req simulateDrawRequest
		_ = c.BodyParser(&req)

		var hackathonID uuid.UUID
		if err := h.db.Pool.QueryRow(c.Context(),
			`SELECT hackathon_id FROM hackathon_issues WHERE id = $1`, issueID).Scan(&hackathonID); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "issue_not_found"})
		}

		res, err := hackathon.RunDraw(c.Context(), h.db.Pool, hackathonID, issueID, req.Seed, true)
		if err != nil {
			slog.Error("hackathon simulate draw", "issue_id", issueID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "simulate_failed"})
		}
		return c.JSON(res)
	}
}

type drawDTO struct {
	ID                 uuid.UUID             `json:"id"`
	IssueID            uuid.UUID             `json:"hackathon_issue_id"`
	RepoFullName       string                `json:"repo_full_name"`
	IssueNumber        int                   `json:"issue_number"`
	Seed               int64                 `json:"seed"`
	Pool               []hackathon.Candidate `json:"pool"`
	PoolSize           int                   `json:"pool_size"`
	WinnerUserID       *uuid.UUID            `json:"winner_user_id"`
	WinnerLogin        *string               `json:"winner_login"`
	UsedWeakPool       bool                  `json:"used_weak_pool"`
	ReservationApplied bool                  `json:"reservation_applied"`
	ReservationFellBk  bool                  `json:"reservation_fell_back"`
	FirstComeFallback  bool                  `json:"first_come_fallback"`
	NoWinnerReason     *string               `json:"no_winner_reason"`
	IsSimulation       bool                  `json:"is_simulation"`
	CreatedAt          time.Time             `json:"created_at"`
}

// ListDraws handles GET /admin/hackathons/:id/draws?issue_id=&include_simulations=.
func (h *AdminHackathonDrawsHandler) ListDraws() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		args := []any{hackathonID}
		query := `
SELECT d.id, d.hackathon_issue_id, p.github_full_name, hi.issue_number, d.seed, d.pool, d.pool_size,
       d.winner_user_id, ga.login, d.used_weak_pool, d.reservation_applied, d.reservation_fell_back,
       d.first_come_fallback, d.no_winner_reason, d.is_simulation, d.created_at
FROM hackathon_draws d
JOIN hackathon_issues hi ON hi.id = d.hackathon_issue_id
JOIN projects p ON p.id = hi.project_id
LEFT JOIN github_accounts ga ON ga.user_id = d.winner_user_id
WHERE d.hackathon_id = $1`
		if issueID := c.Query("issue_id"); issueID != "" {
			if id, err := uuid.Parse(issueID); err == nil {
				args = append(args, id)
				query += " AND d.hackathon_issue_id = $2"
			}
		}
		if c.Query("include_simulations") != "true" {
			query += " AND NOT d.is_simulation"
		}
		query += " ORDER BY d.created_at DESC LIMIT 200"

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			slog.Error("hackathon list draws", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []drawDTO{}
		for rows.Next() {
			var d drawDTO
			var poolRaw []byte
			if err := rows.Scan(&d.ID, &d.IssueID, &d.RepoFullName, &d.IssueNumber, &d.Seed, &poolRaw,
				&d.PoolSize, &d.WinnerUserID, &d.WinnerLogin, &d.UsedWeakPool, &d.ReservationApplied,
				&d.ReservationFellBk, &d.FirstComeFallback, &d.NoWinnerReason, &d.IsSimulation,
				&d.CreatedAt); err != nil {
				slog.Error("hackathon list draws: scan", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			_ = json.Unmarshal(poolRaw, &d.Pool)
			out = append(out, d)
		}
		return c.JSON(fiber.Map{"draws": out})
	}
}

type adminAssignmentDTO struct {
	ID               uuid.UUID       `json:"id"`
	IssueID          uuid.UUID       `json:"hackathon_issue_id"`
	RepoFullName     string          `json:"repo_full_name"`
	IssueNumber      int             `json:"issue_number"`
	GitHubLogin      string          `json:"github_login"`
	OrgLogin         string          `json:"org_login"`
	Status           string          `json:"status"`
	HoldsSlot        bool            `json:"holds_slot"`
	AssignedAt       time.Time       `json:"assigned_at"`
	StaleAt          *time.Time      `json:"stale_at"`
	QualifyingPR     *int            `json:"qualifying_pr_number"`
	ReleaseReason    *string         `json:"release_reason"`
	AbandonRecorded  bool            `json:"abandon_recorded"`
	PriorAssociation json.RawMessage `json:"prior_association"`
}

// ListAssignments handles GET /admin/hackathons/:id/assignments?status=.
// Surfaces §4.2's prior-association evidence alongside each assignment,
// which is the whole point of computing a signal that never blocks.
func (h *AdminHackathonDrawsHandler) ListAssignments() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		args := []any{hackathonID}
		query := `
SELECT a.id, a.hackathon_issue_id, p.github_full_name, a.issue_number, a.github_login, a.org_login,
       a.status, a.holds_slot, a.assigned_at, a.stale_at, a.qualifying_pr_number, a.release_reason,
       a.abandon_recorded, COALESCE(a.prior_association, 'null'::jsonb)
FROM hackathon_assignments a
JOIN projects p ON p.id = a.project_id
WHERE a.hackathon_id = $1`
		if status := c.Query("status"); status != "" {
			args = append(args, status)
			query += " AND a.status = $2"
		}
		query += " ORDER BY a.assigned_at DESC LIMIT 500"

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			slog.Error("hackathon list assignments", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []adminAssignmentDTO{}
		for rows.Next() {
			var d adminAssignmentDTO
			if err := rows.Scan(&d.ID, &d.IssueID, &d.RepoFullName, &d.IssueNumber, &d.GitHubLogin,
				&d.OrgLogin, &d.Status, &d.HoldsSlot, &d.AssignedAt, &d.StaleAt, &d.QualifyingPR,
				&d.ReleaseReason, &d.AbandonRecorded, &d.PriorAssociation); err != nil {
				slog.Error("hackathon list assignments: scan", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, d)
		}
		return c.JSON(fiber.Map{"assignments": out})
	}
}
