package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/founding"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Following Grainlify is an **eligibility requirement** for the Founding
// Contributor Pool, worth zero shares.
//
// It used to pay 500 points. Paying real money for a free, reversible action
// a bot can perform was the same mistake the whole redesign exists to undo -
// and it came out of the budget that now pays people who ship code. As a
// requirement it costs nothing, gets more follows rather than fewer (everyone
// who wants a share must follow), and cannot be farmed, because there is
// nothing to collect.
//
// Nothing in this file grants anything. If a future change adds a reward
// here, it is reintroducing the retired programme.

// socialFollowPlatforms is the set a contributor must submit proof for. Both
// are required and both are submitted together - see SubmitAll.
//
// Keep in sync with the frontend's SOCIAL_FOLLOW_PLATFORMS.
var socialFollowPlatforms = []string{"linkedin", "x"}

// maxScreenshotDataURLBytes bounds each base64 data URL string (screenshots
// are stored as TEXT, same convention as ecosystems.logo_url) - roughly a 5MB
// image after base64's ~1.37x inflation, with headroom.
const maxScreenshotDataURLBytes = 8 * 1024 * 1024

// Submission states. 'revoked' is deliberately distinct from 'rejected':
// rejected means the proof was not good enough, revoked means it was accepted
// and has since been withdrawn. Only the second needs explaining to somebody
// who thought they were eligible.
const (
	socialFollowPending  = "pending"
	socialFollowApproved = "approved"
	socialFollowRejected = "rejected"
	socialFollowRevoked  = "revoked"
)

type SocialFollowHandler struct {
	db     *db.DB
	notify *notifications.Service
}

func NewSocialFollowHandler(d *db.DB, notify *notifications.Service) *SocialFollowHandler {
	return &SocialFollowHandler{db: d, notify: notify}
}

func (h *SocialFollowHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func isImageDataURL(s string) bool {
	return strings.HasPrefix(s, "data:image/")
}

type socialFollowSubmitRequest struct {
	LinkedIn string `json:"linkedin_screenshot"`
	X        string `json:"x_screenshot"`
}

// SubmitAll handles POST /social-follow/submit: both platforms, one request,
// all or nothing.
//
// Atomicity is the point. Under the previous per-platform endpoint a
// contributor could be approved on one platform and pending on another, which
// meant "are they eligible?" had no single answer and a reviewer had to make
// two decisions that only made sense together. Both screenshots arrive in one
// request, land in one row, and are decided once.
//
// Resubmission after a rejection or a revocation overwrites the same row and
// returns it to pending. Nothing is lost by that: every decision ever taken is
// in social_follow_decisions, which is what a dispute is answered from.
func (h *SocialFollowHandler) SubmitAll() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req socialFollowSubmitRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		linkedIn, x := strings.TrimSpace(req.LinkedIn), strings.TrimSpace(req.X)

		// Validate both before writing either. Refusing the whole request is
		// the entire reason this endpoint exists: accepting the valid half
		// would recreate the partial state the atomic model removes.
		if linkedIn == "" || x == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "both_screenshots_required",
				"message": "Both platforms are submitted together. Upload a screenshot for each before submitting.",
			})
		}
		for _, s := range []string{linkedIn, x} {
			if !isImageDataURL(s) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_screenshot"})
			}
			if len(s) > maxScreenshotDataURLBytes {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "screenshot_too_large"})
			}
		}

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}
		defer func() { _ = tx.Rollback(c.Context()) }()

		var submissionID uuid.UUID
		if err := tx.QueryRow(c.Context(), `
INSERT INTO social_follow_submissions (user_id, linkedin_screenshot, x_screenshot, status)
VALUES ($1, $2, $3, 'pending')
ON CONFLICT (user_id) DO UPDATE
SET linkedin_screenshot = EXCLUDED.linkedin_screenshot,
    x_screenshot        = EXCLUDED.x_screenshot,
    status              = 'pending',
    decided_by          = NULL,
    decided_at          = NULL,
    decision_reason     = NULL,
    updated_at          = now()
RETURNING id
`, userID, linkedIn, x).Scan(&submissionID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}

		if _, err := tx.Exec(c.Context(), `
INSERT INTO social_follow_decisions (submission_id, decision, actor_user_id)
VALUES ($1, 'submitted', $2)
`, submissionID, userID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "submit_failed"})
		}
		return c.JSON(fiber.Map{"id": submissionID, "status": socialFollowPending})
	}
}

// Me handles GET /social-follow/me.
//
// Returns the decision and its reason, including for a revocation. Somebody
// whose eligibility is withdrawn without an explanation reads it as
// arbitrary, and they would be right to.
func (h *SocialFollowHandler) Me() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var (
			status  string
			reason  *string
			code    *string
			decided *string
		)
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT status, decision_reason, reason_code, decided_at::text
FROM social_follow_submissions WHERE user_id = $1
`, userID).Scan(&status, &reason, &code, &decided)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(fiber.Map{
				"platforms": socialFollowPlatforms,
				"submitted": false,
				"status":    nil,
				"eligible":  false,
			})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
		}

		return c.JSON(fiber.Map{
			"platforms":       socialFollowPlatforms,
			"submitted":       true,
			"status":          status,
			"decision_reason": reason,
			"reason_code":     code,
			// Resolved server-side so the contributor's page, the admin queue
			// and the notification read the same words without the frontend
			// holding its own copy of what a code means.
			"decision_text": socialFollowDecisionText(code, derefOrEmpty(reason)),
			"decided_at":    decided,
			// The single question everything else here exists to answer.
			"eligible": status == socialFollowApproved,
		})
	}
}

// Page size for the review queue.
//
// Small on purpose. Each row carries BOTH screenshots as base64 data URLs,
// which measure around 787kB per row in production - so a page is roughly
// 8MB of JSON, and the unpaginated version of this endpoint was returning
// 17MB for 22 pending rows and growing with the queue.
//
// The screenshots have to be here: a reviewer decides by looking at them, and
// the whole point of the atomic model is that both are in view for one
// decision. Bounding the page is what stops the response growing without
// limit; getting the images out of the JSON entirely needs an object store
// and is tracked separately.
const (
	// Both numbers rose when the screenshots left this response.
	//
	// They were 10 and 20, and both were set by payload rather than by what a
	// reviewer wants to see: at ~775kB of base64 per row a page of 20 was
	// ~15MB. Without the screenshots a row is about 160 bytes - login, avatar
	// URL, status, two timestamps - so a page of 50 is ~8KB, and the page size
	// can be set by the queue instead of by the wire.
	//
	// Still capped rather than unbounded, but for a different reason now:
	// bulk-approve refuses a selection larger than one page, and that is what
	// keeps "approve everything on screen" bounded to what somebody could
	// plausibly have looked at.
	socialFollowPageSize    = 50
	socialFollowMaxPageSize = 100
)

// ListSubmissions handles GET /admin/social-follow/submissions.
//
// Returns both screenshots so a reviewer can see them side by side and make
// one decision, rather than judging one platform without the other in view.
//
// Paginated, and the page bound is load-bearing beyond payload size: a
// reviewer can only select what is on screen, so what "on screen" means has
// to be a real, small number rather than "however many are pending".
func (h *SocialFollowHandler) ListSubmissions() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		// Defaults to the review queue; ?status=all for the full picture,
		// which is what a revocation is decided from.
		filter := c.Query("status", socialFollowPending)
		limit := clampSocialFollowLimit(c.QueryInt("limit", socialFollowPageSize))
		offset := c.QueryInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		where := ""
		args := []any{}
		if filter != "all" {
			where = " WHERE s.status = $1"
			args = append(args, filter)
		}

		// Total for the SAME filter, so the UI can say "page 2 of 3" and, more
		// importantly, say how many rows exist that are not on this page.
		var total int
		if err := h.db.Pool.QueryRow(c.Context(),
			`SELECT count(*) FROM social_follow_submissions s`+where, args...).Scan(&total); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}

		query := `
SELECT s.id, s.user_id, COALESCE(ga.login, ''), COALESCE(ga.avatar_url, ''),
       s.status, s.decision_reason, s.reason_code, s.decided_at::text, s.created_at::text,
       s.decided_by, COALESCE(dga.login, '')
FROM social_follow_submissions s
LEFT JOIN github_accounts ga ON ga.user_id = s.user_id
LEFT JOIN github_accounts dga ON dga.user_id = s.decided_by` + where + `
ORDER BY s.created_at ASC
LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
		args = append(args, limit, offset)

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		out := []fiber.Map{}
		for rows.Next() {
			var (
				id, userID                    uuid.UUID
				login, avatarURL, status      string
				reason, reasonCode, decidedAt *string
				createdAt                     string
				decidedBy                     *uuid.UUID
				decidedByLogin                string
			)
			if err := rows.Scan(&id, &userID, &login, &avatarURL, &status, &reason, &reasonCode, &decidedAt, &createdAt,
				&decidedBy, &decidedByLogin); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
			}
			out = append(out, fiber.Map{
				"id": id, "user_id": userID, "github_login": login,
				// The avatar is what makes a compact row identifiable at a
				// glance. The screenshots are deliberately NOT here - they are
				// fetched per submission when a row is expanded. See Proofs.
				"avatar_url": avatarURL,
				"status":     status, "decision_reason": reason,
				"reason_code":  reasonCode,
				"reason_label": socialFollowReasonLabel(reasonCode),
				"decided_at":   decidedAt, "created_at": createdAt,
				// Who decided. The trail was already recorded on the row and in
				// social_follow_decisions; it just was not readable from here,
				// so the admin page could show a decision with no author.
				"decided_by":       decidedBy,
				"decided_by_login": decidedByLogin,
			})
		}
		if err := rows.Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}

		return c.JSON(fiber.Map{
			"submissions": out,
			"total":       total,
			"limit":       limit,
			"offset":      offset,
			// Explicit rather than left to the caller to derive from
			// offset+len(submissions) < total. A bulk action's confirmation
			// copy depends on this being right.
			"has_more": offset+len(out) < total,
		})
	}
}

// clampSocialFollowLimit keeps a caller-supplied page size inside sane bounds.
// A limit of 0 or less means "unset", not "no rows", and anything above the
// maximum is capped rather than rejected - the point is that no request can
// ask for the whole queue in one response again.
func clampSocialFollowLimit(n int) int {
	if n <= 0 {
		return socialFollowPageSize
	}
	if n > socialFollowMaxPageSize {
		return socialFollowMaxPageSize
	}
	return n
}

type socialFollowDecisionRequest struct {
	// ReasonCode is one of socialFollowReasons. Optional for backwards
	// compatibility: an admin bundle cached across the deploy still sends a
	// bare Reason, and refusing it would break reviewing until every tab
	// reloaded.
	ReasonCode string `json:"reason_code"`
	// Reason is the free-text note. Required when no code is given (the legacy
	// path) and when the code is 'other', which says nothing on its own.
	Reason string `json:"reason"`
}

// applyDecision is the whole state transition for one submission, inside one
// transaction: guard, update, log.
//
// Extracted from the HTTP handler so bulk approval runs exactly the same rule
// per row rather than a second copy of it. A bulk path that guarded
// differently from the single path is the shape this codebase keeps getting
// caught by, and here it would mean a queue action doing something the row
// button would have refused.
type socialFollowDecisionOutcome struct {
	SubmitterID uuid.UUID
	// ApprovedCount is set only on a cap refusal, so the message can state the
	// actual number rather than restating the constant back at the admin.
	ApprovedCount int
	// PriorStatus is what the row was before, for reporting a skip honestly:
	// "already approved" and "already rejected" are different facts and a
	// reviewer wants to know which.
	PriorStatus string
}

// errSocialFollowNotActionable means the row exists but is not in a state this
// decision applies to. Distinct from "not found" and from a real failure - all
// three are different things to tell an admin.
var (
	errSocialFollowNotActionable = errors.New("submission is not in an actionable state")
	errSocialFollowNotFound      = errors.New("submission not found")
	// The cap is reached. Its own sentinel rather than a generic failure,
	// because it is a deliberate refusal with a specific answer for the admin
	// - and because the bulk loop has to report it per row rather than
	// aborting the batch.
	errSocialFollowCapReached = errors.New("approval cap reached")
)

// socialFollowApprovalCap bounds how many people can be admitted to the
// Founding Contributor Pool by approval.
//
// Enforced rather than intended, which is the whole distinction: it is checked
// inside applyDecision, in the same transaction and under the same advisory
// lock as the write, so two admins approving simultaneously cannot both read
// 299 and both commit. Checking it in the handler before the transaction would
// produce exactly that, and would look correct in every test that ran one
// request at a time.
//
// The bulk path gets per-row enforcement for free by calling applyDecision per
// row. A single check before the loop would admit a whole batch against the
// last remaining slot.
const socialFollowApprovalCap = 300

func (h *SocialFollowHandler) applyDecision(
	ctx context.Context, submissionID, adminID uuid.UUID, to, reasonCode, note string,
) (socialFollowDecisionOutcome, error) {
	tx, err := h.db.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return socialFollowDecisionOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read the current status first so a refusal can say what the row actually
	// was. FOR UPDATE because two admins working the same queue is the normal
	// case, not the exotic one, and without it both can approve the same row
	// and both get told they did it.
	var prior string
	var submitterID uuid.UUID
	if err := tx.QueryRow(ctx, `
SELECT status, user_id FROM social_follow_submissions WHERE id = $1 FOR UPDATE
`, submissionID).Scan(&prior, &submitterID); errors.Is(err, pgx.ErrNoRows) {
		return socialFollowDecisionOutcome{}, errSocialFollowNotFound
	} else if err != nil {
		return socialFollowDecisionOutcome{}, err
	}

	// What each decision may act on.
	//
	// Approve and reject were previously unguarded: acting on a stale row
	// silently overwrote whatever decision was already there, fired a fresh
	// notification, and left nothing indicating it had happened. Rare with a
	// single button; likely the moment a reviewer selects a page and acts on
	// all of it, because the queue moves underneath them.
	if !socialFollowCanTransition(prior, to) {
		return socialFollowDecisionOutcome{PriorStatus: prior}, errSocialFollowNotActionable
	}

	// The cap, inside the transaction.
	//
	// The advisory lock is what makes the count trustworthy: without it two
	// concurrent approvals both read 299, both pass, and both commit, so the
	// cap is exceeded by exactly the number of admins working at once. Same
	// technique the founding wave allocator uses, and for the same reason -
	// a number that must be true rather than usually true.
	//
	// Only approvals are counted and only approvals are refused; a rejection
	// or a revocation is always allowed, since neither admits anybody.
	if to == socialFollowApproved {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('social_follow_approval_cap', 0))`); err != nil {
			return socialFollowDecisionOutcome{PriorStatus: prior}, err
		}
		var approvedNow int
		if err := tx.QueryRow(ctx, `
SELECT count(*)::int FROM social_follow_submissions WHERE status = 'approved'
`).Scan(&approvedNow); err != nil {
			return socialFollowDecisionOutcome{PriorStatus: prior}, err
		}
		if approvedNow >= socialFollowApprovalCap {
			return socialFollowDecisionOutcome{PriorStatus: prior, ApprovedCount: approvedNow}, errSocialFollowCapReached
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE social_follow_submissions
SET status = $1, decided_by = $2, decided_at = now(),
    decision_reason = NULLIF($3, ''), reason_code = NULLIF($4, ''), updated_at = now()
WHERE id = $5
`, to, adminID, note, reasonCode, submissionID); err != nil {
		return socialFollowDecisionOutcome{PriorStatus: prior}, err
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO social_follow_decisions (submission_id, decision, reason, reason_code, actor_user_id)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5)
`, submissionID, to, note, reasonCode, adminID); err != nil {
		return socialFollowDecisionOutcome{PriorStatus: prior}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return socialFollowDecisionOutcome{PriorStatus: prior}, err
	}
	return socialFollowDecisionOutcome{SubmitterID: submitterID, PriorStatus: prior}, nil
}

// socialFollowCanTransition is the single definition of which decisions apply
// to which current state.
//
//	pending  -> approved, rejected     a queued submission gets decided
//	approved -> revoked                eligibility is withdrawn after the fact
//
// Everything else is refused. Re-approving an approval is a no-op dressed up
// as an action; approving something already rejected silently reverses a
// decision somebody made for a reason.
func socialFollowCanTransition(from, to string) bool {
	switch to {
	case socialFollowApproved, socialFollowRejected:
		return from == socialFollowPending
	case socialFollowRevoked:
		return from == socialFollowApproved
	}
	return false
}

// decide applies one decision to a whole submission and logs it.
//
// One decision covers both platforms by construction - there is no per-
// platform state left to disagree with itself.
func (h *SocialFollowHandler) decide(c *fiber.Ctx, to string, requireReason bool) error {
	if h.db == nil || h.db.Pool == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
	}
	adminID, ok := h.userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
	}
	submissionID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_submission_id"})
	}

	var req socialFollowDecisionRequest
	_ = c.BodyParser(&req)
	code, note, verr := validateSocialFollowReason(req.ReasonCode, req.Reason, requireReason)
	if verr != nil {
		return c.Status(fiber.StatusBadRequest).JSON(verr)
	}

	outcome, err := h.applyDecision(c.Context(), submissionID, adminID, to, code, note)
	switch {
	case errors.Is(err, errSocialFollowNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "submission_not_found"})
	case errors.Is(err, errSocialFollowNotActionable):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":          "not_actionable",
			"current_status": outcome.PriorStatus,
			"message":        socialFollowNotActionableMessage(to, outcome.PriorStatus),
		})
	case errors.Is(err, errSocialFollowCapReached):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":    "approval_cap_reached",
			"cap":      socialFollowApprovalCap,
			"approved": outcome.ApprovedCount,
			"message": fmt.Sprintf(
				"The Founding Contributor Pool is full: %d of %d approvals used. Nobody else can be admitted, "+
					"and this submission was left as it was.", outcome.ApprovedCount, socialFollowApprovalCap),
		})
	case err != nil:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "decision_failed"})
	}

	h.afterDecision(c, outcome.SubmitterID, to, socialFollowDecisionText(nullableString(code), note))
	return c.JSON(fiber.Map{"ok": true, "status": to})
}

func socialFollowNotActionableMessage(to, prior string) string {
	if to == socialFollowRevoked {
		return "Only an approved submission can be revoked. This one is " + prior + "."
	}
	return "This submission is already " + prior + ", so it was not changed. Reload the queue to see its current state."
}

// validateSocialFollowReason checks the code/note pair and returns what to
// store. Returns a ready-to-send error body rather than an error string,
// because each case needs its own message.
func validateSocialFollowReason(rawCode, rawNote string, required bool) (string, string, fiber.Map) {
	code := strings.TrimSpace(rawCode)
	note := strings.TrimSpace(rawNote)

	if code == "" {
		// Legacy path: no code, so the note carries the whole decision and has
		// to be there when a reason is required at all.
		if required && note == "" {
			return "", "", fiber.Map{
				"error":   "reason_required",
				"message": "A reason is recorded and shown to the contributor, so this decision needs one.",
			}
		}
		return "", note, nil
	}

	reason, ok := socialFollowReasonByCode(code)
	if !ok {
		return "", "", fiber.Map{
			"error":   "invalid_reason_code",
			"message": "That is not one of the rejection reasons.",
		}
	}
	// 'other' names no problem, so without a note the contributor is told
	// their proof was rejected for "Other" - which is worse than no reason,
	// because it looks like an answer.
	if reason.NeedsNote && note == "" {
		return "", "", fiber.Map{
			"error":   "note_required",
			"message": "\"Other\" needs a note saying what was wrong - the contributor sees it.",
		}
	}
	return code, note, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// notifyDecision tells the contributor what happened. Best-effort: a failed
// notification must not undo a recorded decision.
//
// A revocation is the one that matters. Eligibility disappearing silently,
// and only becoming visible when the pool is shared out, is exactly how a
// fair decision comes to look arbitrary.
// afterDecision runs every post-commit side effect of one decision.
//
// Both the single-decision handler and the bulk loop call this, so an effect
// added here cannot reach one path and miss the other - which is the shape
// that has produced most of this codebase's worst bugs. The bulk loop calls it
// per row for the same reason.
func (h *SocialFollowHandler) afterDecision(c *fiber.Ctx, userID uuid.UUID, to, reason string) {
	h.notifyDecision(c, userID, to, reason)

	// An approval is now the other half of wave assignment. Somebody who
	// verified before being approved has no position; approving them gives
	// them one. Whichever of (verify, approve) happens second does the work,
	// and OnSocialFollowApproved is idempotent, so an approval for an
	// already-assigned member is a no-op.
	//
	// Best-effort and after the commit: the approval is durable regardless.
	if to == socialFollowApproved && h.db != nil && h.db.Pool != nil {
		cfg, err := hackathon.EffectiveValues(c.Context(), h.db.Pool, nil)
		if err != nil {
			slog.Warn("social_follow: config load failed, wave not assigned", "user_id", userID, "error", err)
			return
		}
		founding.OnSocialFollowApproved(c.Context(), h.db.Pool, userID, cfg)
	}
}

func (h *SocialFollowHandler) notifyDecision(c *fiber.Ctx, userID uuid.UUID, to, reason string) {
	if h.notify == nil {
		return
	}
	var title, body string
	switch to {
	case socialFollowApproved:
		title = "Social follow approved"
		body = "Your follow proof was approved. You're eligible for the Founding Contributor Pool."
	case socialFollowRejected:
		title = "Social follow proof needs another look"
		body = "Your follow proof wasn't approved: " + reason + " You can upload new screenshots and submit again."
	case socialFollowRevoked:
		title = "Social follow eligibility withdrawn"
		body = "Your follow approval was withdrawn: " + reason + " You can submit new proof to become eligible again."
	default:
		return
	}
	h.notify.Notify(c.Context(), userID, notifications.TypeSocialFollowCompleted, title, body, notifications.SettingsLink(notifications.SubtabRewards))
}

// Approve handles POST /admin/social-follow/submissions/:id/approve.
func (h *SocialFollowHandler) Approve() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowApproved, false) }
}

// Reject handles POST /admin/social-follow/submissions/:id/reject. A reason is
// required - it is shown to the contributor so they can fix and resubmit.
func (h *SocialFollowHandler) Reject() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowRejected, true) }
}

// Revoke handles POST /admin/social-follow/submissions/:id/revoke.
//
// Eligibility is re-read at settlement, so an approval must be withdrawable
// after the fact. The submission and both screenshots are kept: this is a
// status change, and the record of what was approved is precisely what a
// revocation dispute turns on.
func (h *SocialFollowHandler) Revoke() fiber.Handler {
	return func(c *fiber.Ctx) error { return h.decide(c, socialFollowRevoked, true) }
}

type socialFollowBulkRequest struct {
	IDs []string `json:"ids"`
}

// BulkApprove handles POST /admin/social-follow/submissions/bulk-approve.
//
// # Why the batch is capped at a page
//
// An admin must not be able to approve submissions they have not looked at.
// Approval grants Founding Contributor Pool eligibility, so "select everything
// pending and press approve" is a way to hand out eligibility without seeing
// the proof. The UI only offers selection over the rows on screen; this cap is
// what makes that a rule rather than a convention, since the endpoint is
// reachable without the UI.
//
// # Why every row is its own transaction
//
// One stale row must not discard nineteen valid approvals. Wrapping the batch
// in a single transaction would mean the whole thing rolls back because one
// submission was resubmitted while the reviewer was reading, which is both
// annoying and misleading - the nineteen really were fine.
//
// # Why the response has three lists
//
//	approved  the decision was applied
//	skipped   the row was not in a state to be approved, or is gone
//	failed    something went wrong and it is worth retrying
//
// Skipped and failed are deliberately not merged. A skip is the system working
// - the queue moved under the reviewer - and needs no action. A failure is the
// system not working. Reporting "3 failed" for three rows that were simply
// already approved sends somebody looking for a bug that is not there, and
// reporting "20 approved" when 3 were not is the lie this endpoint must never
// tell.
func (h *SocialFollowHandler) BulkApprove() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		adminID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req socialFollowBulkRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		// Dedupe before the cap, so the same id sent twice cannot push a
		// legitimate selection over the limit.
		seen := map[uuid.UUID]bool{}
		ids := []uuid.UUID{}
		for _, raw := range req.IDs {
			id, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_submission_id"})
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "no_submissions_selected",
				"message": "Select at least one submission.",
			})
		}
		if len(ids) > socialFollowMaxPageSize {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "too_many_submissions",
				"message": "Approve at most one page at a time - eligibility should only be granted for proof somebody has looked at.",
				"max":     socialFollowMaxPageSize,
			})
		}

		approved := []fiber.Map{}
		skipped := []fiber.Map{}
		failed := []fiber.Map{}
		notify := []uuid.UUID{}

		for _, id := range ids {
			outcome, err := h.applyDecision(c.Context(), id, adminID, socialFollowApproved, "", "")
			switch {
			case errors.Is(err, errSocialFollowNotFound):
				skipped = append(skipped, fiber.Map{"id": id, "reason": "not_found"})
			case errors.Is(err, errSocialFollowNotActionable):
				skipped = append(skipped, fiber.Map{
					"id": id, "reason": "not_pending", "current_status": outcome.PriorStatus,
				})
			case errors.Is(err, errSocialFollowCapReached):
				// Skipped, not failed, and the loop continues rather than
				// aborting: every remaining row hits the same wall, and the
				// admin needs the whole list to know none of them landed.
				skipped = append(skipped, fiber.Map{
					"id": id, "reason": "cap_reached", "approved": outcome.ApprovedCount,
				})
			case err != nil:
				slog.Error("social_follow: bulk approve failed for one submission",
					"submission_id", id, "admin_id", adminID, "error", err)
				failed = append(failed, fiber.Map{"id": id})
			default:
				approved = append(approved, fiber.Map{"id": id})
				notify = append(notify, outcome.SubmitterID)
			}
		}

		// After every decision is durable. A notification failure must not make
		// an approval look like it did not happen.
		for _, submitter := range notify {
			h.afterDecision(c, submitter, socialFollowApproved, "")
		}

		return c.JSON(fiber.Map{
			"approved": approved,
			"skipped":  skipped,
			"failed":   failed,
			// Counts alongside the lists so the UI's summary line cannot drift
			// from the lists it is summarising.
			"approved_count": len(approved),
			"skipped_count":  len(skipped),
			"failed_count":   len(failed),
		})
	}
}

// ReasonCodes handles GET /admin/social-follow/reason-codes.
//
// The picker is built from this rather than from a list in the frontend, so
// the codes and their wording exist once. A TypeScript copy would be the same
// rule written down twice, and the contributor's status page, the notification
// and the admin picker would then be free to disagree about what a code means.
func (h *SocialFollowHandler) ReasonCodes() fiber.Handler {
	return func(c *fiber.Ctx) error {
		out := make([]fiber.Map, 0, len(socialFollowReasons))
		for _, r := range socialFollowReasons {
			out = append(out, fiber.Map{"code": r.Code, "label": r.Label, "needs_note": r.NeedsNote})
		}
		return c.JSON(fiber.Map{"reason_codes": out})
	}
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Proofs handles GET /admin/social-follow/submissions/:id/proofs.
//
// The two screenshots for one submission, fetched when a reviewer expands a
// row rather than shipped with every row of the queue.
//
// Why this exists: the screenshots are base64 data URLs in TEXT columns,
// ~775kB a row, and returning them with the list made a page of ten a 7.7MB
// response that grew with the queue. Paging bounded it; not sending them at
// all removes it. A reviewer opens a handful of rows per session, so the
// proofs are fetched a handful of times instead of fifty.
//
// The atomic rule still holds: both screenshots come back together, in one
// response, because a decision covers both platforms and judging one without
// the other in view is half a decision. There is deliberately no endpoint for
// a single platform's proof.
//
// Admin-only, same as the list. These are photographs of people's social
// accounts; the fact that they are keyed by an unguessable UUID is not
// authorisation.
func (h *SocialFollowHandler) Proofs() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		submissionID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_submission_id"})
		}

		var linkedIn, x string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT linkedin_screenshot, x_screenshot
FROM social_follow_submissions WHERE id = $1
`, submissionID).Scan(&linkedIn, &x)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "submission_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
		}

		return c.JSON(fiber.Map{
			"id":                  submissionID,
			"linkedin_screenshot": linkedIn,
			"x_screenshot":        x,
		})
	}
}
