package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// AdminHackathonVerdictsHandler is the human review interface for §5.
//
// In shadow mode - the default, and how AI-specs.md §12 says event 1 should
// run - every verdict is reviewed by hand, so this is the primary interface
// rather than an exception path.
type AdminHackathonVerdictsHandler struct {
	db *db.DB
}

func NewAdminHackathonVerdictsHandler(d *db.DB) *AdminHackathonVerdictsHandler {
	return &AdminHackathonVerdictsHandler{db: d}
}

type verdictDTO struct {
	ID              uuid.UUID       `json:"id"`
	HackathonID     uuid.UUID       `json:"hackathon_id"`
	IssueID         *uuid.UUID      `json:"hackathon_issue_id"`
	ProjectID       uuid.UUID       `json:"project_id"`
	RepoFullName    string          `json:"repo_full_name"`
	PRNumber        int             `json:"pr_number"`
	IssueNumber     *int            `json:"issue_number"`
	GitHubLogin     string          `json:"github_login"`
	PrefilterStatus string          `json:"prefilter_status"`
	PrefilterReason *string         `json:"prefilter_reason"`
	DiffStats       json.RawMessage `json:"diff_stats"`

	DuplicateOfVerdictID *uuid.UUID `json:"duplicate_of_verdict_id"`
	DuplicateSimilarity  *float64   `json:"duplicate_similarity"`
	DuplicateFlagged     bool       `json:"duplicate_flagged"`

	JudgeBucket       *string         `json:"judge_bucket"`
	JudgeConfidence   *string         `json:"judge_confidence"`
	JudgePayload      json.RawMessage `json:"judge_payload"`
	JudgeModel        *string         `json:"judge_model"`
	CrossCheckBucket  *string         `json:"cross_check_bucket"`
	CrossCheckPayload json.RawMessage `json:"cross_check_payload"`
	CrossCheckModel   *string         `json:"cross_check_model"`
	EscalationBucket  *string         `json:"escalation_bucket"`
	EscalationPayload json.RawMessage `json:"escalation_payload"`

	NeedsHumanReview bool       `json:"needs_human_review"`
	ReviewReason     *string    `json:"review_reason"`
	FinalBucket      *string    `json:"final_bucket"`
	FinalSource      *string    `json:"final_source"`
	OverriddenBy     *uuid.UUID `json:"overridden_by"`
	OverrideReason   *string    `json:"override_reason"`
	OverriddenAt     *time.Time `json:"overridden_at"`

	Units        *int      `json:"units"`
	PayoutAmount *string   `json:"payout_amount"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// MergeCommitSHA lets the UI build citation links that point at the diff
	// as it was merged, rather than at a branch that has since moved.
	MergeCommitSHA *string `json:"merge_commit_sha"`
}

const verdictSelectCols = `
  v.id, v.hackathon_id, v.hackathon_issue_id, v.project_id, p.github_full_name, v.pr_number,
  hi.issue_number, v.github_login, v.prefilter_status, v.prefilter_reason,
  COALESCE(v.diff_stats, 'null'::jsonb),
  v.duplicate_of_verdict_id, v.duplicate_similarity, v.duplicate_flagged,
  v.judge_bucket, v.judge_confidence, COALESCE(v.judge_payload, 'null'::jsonb), v.judge_model,
  v.cross_check_bucket, COALESCE(v.cross_check_payload, 'null'::jsonb), v.cross_check_model,
  v.escalation_bucket, COALESCE(v.escalation_payload, 'null'::jsonb),
  v.needs_human_review, v.review_reason, v.final_bucket, v.final_source,
  v.overridden_by, v.override_reason, v.overridden_at,
  v.units, v.payout_amount::text, v.created_at, v.updated_at, pr.merge_commit_sha`

func scanVerdict(row interface{ Scan(...any) error }) (verdictDTO, error) {
	var v verdictDTO
	err := row.Scan(&v.ID, &v.HackathonID, &v.IssueID, &v.ProjectID, &v.RepoFullName, &v.PRNumber,
		&v.IssueNumber, &v.GitHubLogin, &v.PrefilterStatus, &v.PrefilterReason, &v.DiffStats,
		&v.DuplicateOfVerdictID, &v.DuplicateSimilarity, &v.DuplicateFlagged,
		&v.JudgeBucket, &v.JudgeConfidence, &v.JudgePayload, &v.JudgeModel,
		&v.CrossCheckBucket, &v.CrossCheckPayload, &v.CrossCheckModel,
		&v.EscalationBucket, &v.EscalationPayload,
		&v.NeedsHumanReview, &v.ReviewReason, &v.FinalBucket, &v.FinalSource,
		&v.OverriddenBy, &v.OverrideReason, &v.OverriddenAt,
		&v.Units, &v.PayoutAmount, &v.CreatedAt, &v.UpdatedAt, &v.MergeCommitSHA)
	return v, err
}

const verdictFrom = `
FROM hackathon_verdicts v
JOIN projects p ON p.id = v.project_id
LEFT JOIN hackathon_issues hi ON hi.id = v.hackathon_issue_id
LEFT JOIN github_pull_requests pr ON pr.project_id = v.project_id AND pr.number = v.pr_number`

// List handles GET /admin/hackathons/:id/verdicts?status=&bucket=.
//
// status=needs_review is the queue that matters in shadow mode: everything
// a human still has to look at.
func (h *AdminHackathonVerdictsHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		query := `SELECT` + verdictSelectCols + verdictFrom + ` WHERE v.hackathon_id = $1`
		args := []any{hackathonID}

		switch c.Query("status") {
		case "needs_review":
			// Ordinary review only. Cases the adjudicator could not resolve
			// are excluded here and have their own filter below, because
			// they are a different finding and get lost in the general queue.
			query += ` AND (v.needs_human_review OR v.duplicate_flagged)` +
				` AND (v.review_reason IS DISTINCT FROM '` + hackathon.ReviewReasonUnresolvable + `')`
		case "unresolvable":
			// §5.6/§5.7: the adjudicator saw both reviews and could not say
			// which was better supported. That points at the bucket
			// definitions rather than at this pull request.
			query += ` AND v.review_reason = '` + hackathon.ReviewReasonUnresolvable + `'`
		case "rejected":
			query += ` AND v.prefilter_status = 'rejected'`
		case "overridden":
			query += ` AND v.overridden_at IS NOT NULL`
		}
		if bucket := c.Query("bucket"); bucket != "" {
			args = append(args, bucket)
			query += ` AND v.final_bucket = $2`
		}
		query += ` ORDER BY (v.review_reason = 'escalation_could_not_resolve') DESC, v.needs_human_review DESC, v.duplicate_flagged DESC, v.created_at DESC LIMIT 500`

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			slog.Error("hackathon verdicts list", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []verdictDTO{}
		for rows.Next() {
			v, err := scanVerdict(rows)
			if err != nil {
				slog.Error("hackathon verdicts list: scan", "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, v)
		}

		// Stats are returned with the list rather than behind a separate
		// call: the disagreement rate is a number someone should see every
		// time they open the queue, not one they have to go looking for.
		stats, statsErr := hackathon.ComputeJudgingStats(c.Context(), h.db.Pool, hackathonID)
		if statsErr != nil {
			slog.Warn("hackathon verdicts: stats", "error", statsErr)
		}

		// Shadow mode is reported alongside the list so the UI can say
		// plainly that nothing here has been shown to anyone.
		return c.JSON(fiber.Map{
			"verdicts":    out,
			"shadow_mode": hackathon.ShadowMode(c.Context(), h.db.Pool, hackathonID),
			"stats":       stats,
		})
	}
}

// Get handles GET /admin/hackathon-verdicts/:id - one complete verdict.
func (h *AdminHackathonVerdictsHandler) Get() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_verdict_id"})
		}
		row := h.db.Pool.QueryRow(c.Context(),
			`SELECT`+verdictSelectCols+verdictFrom+` WHERE v.id = $1`, id)
		v, err := scanVerdict(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "verdict_not_found"})
		}
		if err != nil {
			slog.Error("hackathon verdict get", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "load_failed"})
		}
		return c.JSON(fiber.Map{
			"verdict":     v,
			"shadow_mode": hackathon.ShadowMode(c.Context(), h.db.Pool, v.HackathonID),
		})
	}
}

type overrideVerdictRequest struct {
	Bucket string `json:"bucket"`
	Reason string `json:"reason"`
}

var validBuckets = map[string]bool{
	"rejected": true, "accepted": true, "substantial": true, "exceptional": true,
}

// Override handles POST /admin/hackathon-verdicts/:id/override.
//
// The reason is mandatory, and that is not bureaucracy. §5.7: "Every
// override is training data. Add overridden PRs to the calibration set (§8)
// - they are exactly the cases the prompt got wrong, and therefore the most
// valuable examples available." An override with no written reason is a
// example with no label, which is worth nothing later.
func (h *AdminHackathonVerdictsHandler) Override() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_verdict_id"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req overrideVerdictRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		req.Bucket = strings.TrimSpace(req.Bucket)
		req.Reason = strings.TrimSpace(req.Reason)

		if !validBuckets[req.Bucket] {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_bucket"})
		}
		if req.Reason == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "reason_required",
				"message": "A written reason is required. Overrides are the calibration set's most valuable examples, and an override with no reason can't be used as one.",
			})
		}

		tag, err := h.db.Pool.Exec(c.Context(), `
UPDATE hackathon_verdicts
SET final_bucket = $2,
    final_source = 'human_override',
    override_reason = $3,
    overridden_by = $4,
    overridden_at = now(),
    -- The point of a human decision is that it settles the question.
    needs_human_review = false,
    updated_at = now()
WHERE id = $1
`, id, req.Bucket, req.Reason, actorID)
		if err != nil {
			slog.Error("hackathon verdict override", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "override_failed"})
		}
		if tag.RowsAffected() == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "verdict_not_found"})
		}
		return c.JSON(fiber.Map{"ok": true, "final_bucket": req.Bucket})
	}
}
