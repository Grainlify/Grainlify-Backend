package handlers

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// HackathonAppealsHandler serves AI-specs.md §6 to the two people who need
// it: the contributor reading their own verdict, and the admin answering the
// appeal against it.
type HackathonAppealsHandler struct {
	db *db.DB
}

func NewHackathonAppealsHandler(d *db.DB) *HackathonAppealsHandler {
	return &HackathonAppealsHandler{db: d}
}

// appealDTO is the appeal as both sides see it. The contributor's own reason
// and the reviewer's decision reason are both included: §6 makes the human
// decision final *and recorded*, and a decision the appellant cannot read is
// not really recorded.
type appealDTO struct {
	ID             uuid.UUID  `json:"id"`
	VerdictID      uuid.UUID  `json:"verdict_id"`
	GitHubLogin    string     `json:"github_login"`
	Reason         string     `json:"reason"`
	Status         string     `json:"status"`
	DecisionReason *string    `json:"decision_reason"`
	DecidedBucket  *string    `json:"decided_bucket"`
	DecidedAt      *time.Time `json:"decided_at"`
	CreatedAt      time.Time  `json:"created_at"`

	// Only populated on the admin queue - §6: "Appeal routes to a human with
	// both model verdicts and the diff."
	Verdict *verdictDTO `json:"verdict,omitempty"`
}

// MyVerdicts handles GET /grainhack/my-verdicts.
//
// Deliberately returns nothing before Phase 5. §6 opens the contributor view
// when results are published; showing a verdict earlier would leak a bucket
// that is still being reviewed, and start an appeal conversation about a
// number that can still change without anyone appealing.
func (h *HackathonAppealsHandler) MyVerdicts() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT`+verdictSelectCols+`, hk.phase, hk.results_published_at, hk.appeals_closed_at,
       a.id, a.status, a.reason, a.decision_reason, a.decided_bucket, a.decided_at, a.created_at
`+verdictFrom+`
JOIN hackathons hk ON hk.id = v.hackathon_id
LEFT JOIN hackathon_appeals a ON a.verdict_id = v.id
WHERE v.user_id = $1
  AND hk.phase IN ('results_published', 'settled')
ORDER BY v.created_at DESC
`, userID)
		if err != nil {
			slog.Error("my verdicts: query", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "verdicts_fetch_failed"})
		}
		defer rows.Close()

		out := []fiber.Map{}
		for rows.Next() {
			var (
				v                  verdictDTO
				phase              string
				resultsPublishedAt *time.Time
				appealsClosedAt    *time.Time
				appealID           *uuid.UUID
				appealStatus       *string
				appealReason       *string
				decisionReason     *string
				decidedBucket      *string
				decidedAt          *time.Time
				appealCreatedAt    *time.Time
			)
			if err := rows.Scan(
				&v.ID, &v.HackathonID, &v.IssueID, &v.ProjectID, &v.RepoFullName, &v.PRNumber,
				&v.IssueNumber, &v.GitHubLogin, &v.PrefilterStatus, &v.PrefilterReason, &v.DiffStats,
				&v.DuplicateOfVerdictID, &v.DuplicateSimilarity, &v.DuplicateFlagged,
				&v.JudgeBucket, &v.JudgeConfidence, &v.JudgePayload, &v.JudgeModel,
				&v.CrossCheckBucket, &v.CrossCheckPayload, &v.CrossCheckModel,
				&v.EscalationBucket, &v.EscalationPayload,
				&v.NeedsHumanReview, &v.ReviewReason, &v.FinalBucket, &v.FinalSource,
				&v.OverriddenBy, &v.OverrideReason, &v.OverriddenAt,
				&v.Units, &v.PayoutAmount, &v.CreatedAt, &v.UpdatedAt, &v.MergeCommitSHA,
				&phase, &resultsPublishedAt, &appealsClosedAt,
				&appealID, &appealStatus, &appealReason, &decisionReason, &decidedBucket, &decidedAt, &appealCreatedAt,
			); err != nil {
				slog.Error("my verdicts: scan", "error", err)
				continue
			}

			entry := fiber.Map{
				"verdict": v,
				"phase":   phase,
			}
			if appealID != nil {
				entry["appeal"] = appealDTO{
					ID: *appealID, VerdictID: v.ID, GitHubLogin: v.GitHubLogin,
					Reason: derefStr(appealReason), Status: derefStr(appealStatus),
					DecisionReason: decisionReason, DecidedBucket: decidedBucket,
					DecidedAt: decidedAt, CreatedAt: derefTime(appealCreatedAt),
				}
			}
			out = append(out, entry)
		}

		return c.JSON(fiber.Map{"verdicts": out})
	}
}

// AppealWindowForVerdict handles GET /grainhack/verdicts/:id/appeal-window, so
// the UI can say when appeals close instead of only discovering it by being
// refused.
func (h *HackathonAppealsHandler) AppealWindowForVerdict() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		verdictID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_verdict_id"})
		}

		var hackathonID uuid.UUID
		var owner *uuid.UUID
		if err := h.db.Pool.QueryRow(c.Context(),
			`SELECT hackathon_id, user_id FROM hackathon_verdicts WHERE id = $1`, verdictID,
		).Scan(&hackathonID, &owner); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "verdict_not_found"})
		}
		if owner == nil || *owner != userID {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_your_verdict"})
		}

		window, err := hackathon.GetAppealWindowByID(c.Context(), h.db.Pool, hackathonID)
		if err != nil {
			slog.Error("appeal window", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "appeal_window_failed"})
		}
		return c.JSON(window)
	}
}

type submitAppealRequest struct {
	Reason string `json:"reason"`
}

// Appeal handles POST /grainhack/verdicts/:id/appeal.
func (h *HackathonAppealsHandler) Appeal() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		verdictID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_verdict_id"})
		}
		var req submitAppealRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if strings.TrimSpace(req.Reason) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "reason_required",
				"message": "Tell us what you think the review got wrong. A reviewer reads this.",
			})
		}

		id, err := hackathon.SubmitAppeal(c.Context(), h.db.Pool, hackathon.SubmitAppealInput{
			VerdictID: verdictID, UserID: userID, Reason: req.Reason,
		})
		switch {
		case err == nil:
			return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
		case errors.Is(err, hackathon.ErrAppealNotYours):
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_your_verdict", "message": err.Error()})
		case errors.Is(err, hackathon.ErrAppealExists):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "already_appealed", "message": err.Error()})
		case errors.Is(err, hackathon.ErrAppealWindowClosed):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "appeal_window_closed", "message": err.Error()})
		default:
			slog.Error("submit appeal", "error", err)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "appeal_failed", "message": err.Error()})
		}
	}
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

// AdminList handles GET /admin/hackathons/:id/appeals.
//
// Returns the full verdict alongside each appeal, because §6 requires the
// appeal to reach a human "with both model verdicts and the diff" - making
// the reviewer go and fetch that separately is how it ends up not being read.
func (h *HackathonAppealsHandler) AdminList() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		query := `
SELECT a.id, a.verdict_id, a.github_login, a.reason, a.status,
       a.decision_reason, a.decided_bucket, a.decided_at, a.created_at,
       ` + verdictSelectCols + `
FROM hackathon_appeals a
JOIN hackathon_verdicts v ON v.id = a.verdict_id
JOIN projects p ON p.id = v.project_id
LEFT JOIN hackathon_issues hi ON hi.id = v.hackathon_issue_id
LEFT JOIN github_pull_requests pr ON pr.project_id = v.project_id AND pr.number = v.pr_number
WHERE a.hackathon_id = $1`
		args := []any{hackathonID}
		if status := c.Query("status"); status != "" {
			query += ` AND a.status = $2`
			args = append(args, status)
		}
		// Pending first: those are the ones blocking the event from settling.
		query += ` ORDER BY (a.status = 'pending') DESC, a.created_at ASC`

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			slog.Error("admin appeals: query", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "appeals_fetch_failed"})
		}
		defer rows.Close()

		out := []appealDTO{}
		for rows.Next() {
			var a appealDTO
			var v verdictDTO
			if err := rows.Scan(
				&a.ID, &a.VerdictID, &a.GitHubLogin, &a.Reason, &a.Status,
				&a.DecisionReason, &a.DecidedBucket, &a.DecidedAt, &a.CreatedAt,
				&v.ID, &v.HackathonID, &v.IssueID, &v.ProjectID, &v.RepoFullName, &v.PRNumber,
				&v.IssueNumber, &v.GitHubLogin, &v.PrefilterStatus, &v.PrefilterReason, &v.DiffStats,
				&v.DuplicateOfVerdictID, &v.DuplicateSimilarity, &v.DuplicateFlagged,
				&v.JudgeBucket, &v.JudgeConfidence, &v.JudgePayload, &v.JudgeModel,
				&v.CrossCheckBucket, &v.CrossCheckPayload, &v.CrossCheckModel,
				&v.EscalationBucket, &v.EscalationPayload,
				&v.NeedsHumanReview, &v.ReviewReason, &v.FinalBucket, &v.FinalSource,
				&v.OverriddenBy, &v.OverrideReason, &v.OverriddenAt,
				&v.Units, &v.PayoutAmount, &v.CreatedAt, &v.UpdatedAt, &v.MergeCommitSHA,
			); err != nil {
				slog.Error("admin appeals: scan", "error", err)
				continue
			}
			a.Verdict = &v
			out = append(out, a)
		}

		window, err := hackathon.GetAppealWindowByID(c.Context(), h.db.Pool, hackathonID)
		if err != nil {
			slog.Warn("admin appeals: window", "error", err)
		}
		return c.JSON(fiber.Map{"appeals": out, "appeal_window": window})
	}
}

type decideAppealRequest struct {
	Upheld bool   `json:"upheld"`
	Bucket string `json:"bucket"`
	Reason string `json:"reason"`
}

// AdminDecide handles POST /admin/hackathon-appeals/:id/decide.
func (h *HackathonAppealsHandler) AdminDecide() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		reviewerIDStr, _ := c.Locals(auth.LocalUserID).(string)
		reviewerID, err := uuid.Parse(reviewerIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
		}
		appealID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_appeal_id"})
		}
		var req decideAppealRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if strings.TrimSpace(req.Reason) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "reason_required",
				"message": "A written reason is required. The contributor is shown this, and it feeds the calibration set.",
			})
		}

		err = hackathon.DecideAppeal(c.Context(), h.db.Pool, hackathon.AppealDecision{
			AppealID: appealID, ReviewerID: reviewerID,
			Upheld: req.Upheld, NewBucket: strings.TrimSpace(req.Bucket), Reason: req.Reason,
		})
		switch {
		case err == nil:
			return c.JSON(fiber.Map{"ok": true})
		case errors.Is(err, hackathon.ErrAppealDecided):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "already_decided", "message": err.Error()})
		default:
			slog.Error("decide appeal", "error", err)
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "decision_failed", "message": err.Error()})
		}
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
