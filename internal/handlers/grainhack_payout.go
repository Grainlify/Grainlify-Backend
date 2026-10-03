package handlers

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/grainhack"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// GrainHackPayoutHandler serves GrainHack results statements: the admin issues
// one, the bounty agent fetches it and reports back what it paid.
//
// Thin on purpose: every rule lives in internal/grainhack. This file maps
// requests in and refusals out, and holds the two trust boundaries - admin
// routes behind requireAdmin (mounted in api.go), agent routes behind the
// GRAINHACK_STATEMENT_TOKEN bearer checked here.
type GrainHackPayoutHandler struct {
	svc       *grainhack.Service
	configErr error
	token     string
}

// NewGrainHackPayoutHandler never fails the boot. An unset or malformed
// signing key or network leaves the statement routes answering 503 with the
// reason; an unset token leaves the agent routes answering 503.
func NewGrainHackPayoutHandler(d *db.DB, cfg config.Config, notif *notifications.Service) *GrainHackPayoutHandler {
	h := &GrainHackPayoutHandler{token: strings.TrimSpace(cfg.GrainHackStatementToken)}
	if d == nil || d.Pool == nil {
		h.configErr = errors.New("database not configured")
		return h
	}
	var n grainhack.Notifier
	if notif != nil {
		n = notif
	}
	svc, err := grainhack.NewService(d.Pool, cfg.GrainHackResultsSigningKey, cfg.GrainHackPayoutNetwork, n)
	h.svc = svc
	h.configErr = err
	if err == nil && svc.Key == nil {
		h.configErr = errors.New("GRAINHACK_RESULTS_SIGNING_KEY is not set")
	}
	if err != nil {
		slog.Error("grainhack results: signing is off", "error", err)
	}
	slog.Info("grainhack results: configured",
		"signing_key_set", svc.Key != nil, "network", svc.Network, "agent_token_set", h.token != "")
	return h
}

// NewGrainHackPayoutHandlerWith injects a service and token, for tests.
func NewGrainHackPayoutHandlerWith(svc *grainhack.Service, token string) *GrainHackPayoutHandler {
	h := &GrainHackPayoutHandler{svc: svc, token: token}
	if svc == nil || svc.Key == nil {
		h.configErr = errors.New("GRAINHACK_RESULTS_SIGNING_KEY is not set")
	}
	return h
}

func (h *GrainHackPayoutHandler) unconfigured(c *fiber.Ctx) error {
	detail := "unconfigured"
	if h.configErr != nil {
		detail = h.configErr.Error()
	}
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"error": "grainhack_results_unconfigured", "detail": detail,
	})
}

func (h *GrainHackPayoutHandler) ready() bool { return h.svc != nil && h.configErr == nil }

// agentAuthorized checks the agent's bearer token in constant time. Unset
// configuration refuses everything.
func (h *GrainHackPayoutHandler) agentAuthorized(c *fiber.Ctx) (bool, error) {
	if h.token == "" {
		return false, c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "grainhack_agent_token_unconfigured"})
	}
	auth := c.Get("Authorization")
	const prefix = "bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false, c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing_bearer_token"})
	}
	got := strings.TrimSpace(auth[len(prefix):])
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		return false, c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "bad_token"})
	}
	return true, nil
}

// grainhackError maps a refusal to a status and a stable name.
func grainhackError(c *fiber.Ctx, err error) error {
	var missing *grainhack.MissingGitHubError
	var notIn *grainhack.NotInStatementError
	var other *grainhack.OtherRailError
	status, name := fiber.StatusInternalServerError, "grainhack_failed"
	switch {
	case errors.As(err, &missing):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "winners_without_github", "winners": missing.Winners, "detail": err.Error(),
		})
	case errors.As(err, &notIn):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "not_in_statement", "github_user_ids": notIn.GitHubUserIDs, "detail": err.Error(),
		})
	case errors.As(err, &other):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error": "paid_on_other_rail", "rail": other.Rail, "detail": err.Error(),
		})
	case errors.Is(err, grainhack.ErrNotConfigured):
		status, name = fiber.StatusServiceUnavailable, "grainhack_results_unconfigured"
	case errors.Is(err, hackathon.ErrPayoutNotReleasable):
		status, name = fiber.StatusConflict, "payout_not_releasable"
	case errors.Is(err, hackathon.ErrNothingToSettle):
		status, name = fiber.StatusConflict, "nothing_to_settle"
	case errors.Is(err, grainhack.ErrUnsupportedPool):
		status, name = fiber.StatusBadRequest, "pool_unsupported"
	case errors.Is(err, grainhack.ErrNoComputation):
		status, name = fiber.StatusConflict, "no_computation"
	case errors.Is(err, grainhack.ErrPayoutRunNotCurrent):
		status, name = fiber.StatusConflict, "payout_run_not_current"
	case errors.Is(err, grainhack.ErrComputationChanged):
		status, name = fiber.StatusConflict, "computation_changed"
	case errors.Is(err, grainhack.ErrNetworkChanged):
		status, name = fiber.StatusConflict, "network_changed"
	case errors.Is(err, grainhack.ErrSettlementChanged):
		status, name = fiber.StatusConflict, "settlement_changed"
	case errors.Is(err, grainhack.ErrNothingToSupersede):
		status, name = fiber.StatusConflict, "nothing_to_supersede"
	case errors.Is(err, grainhack.ErrConcurrentIssue):
		status, name = fiber.StatusConflict, "concurrent_issue"
	case errors.Is(err, grainhack.ErrNotFound):
		status, name = fiber.StatusNotFound, "not_found"
	case errors.Is(err, grainhack.ErrBadReport):
		status, name = fiber.StatusBadRequest, "bad_request"
	case errors.Is(err, grainhack.ErrNotInStatement):
		status, name = fiber.StatusBadRequest, "not_in_statement"
	case errors.Is(err, grainhack.ErrLineNotPayable):
		status, name = fiber.StatusConflict, "line_not_payable"
	case errors.Is(err, grainhack.ErrReportMismatch):
		status, name = fiber.StatusConflict, "report_mismatch"
	case errors.Is(err, grainhack.ErrConflictingPayment):
		slog.Error("grainhack: a second, different payment was reported for one winner", "error", err)
		status, name = fiber.StatusConflict, "conflicting_payment"
	case errors.Is(err, grainhack.ErrTxAlreadyReported):
		slog.Error("grainhack: one transaction reported for two winners", "error", err)
		status, name = fiber.StatusConflict, "tx_already_reported"
	case errors.Is(err, grainhack.ErrNoticeAlreadySent):
		status, name = fiber.StatusConflict, "already_sent"
	case errors.Is(err, grainhack.ErrNotConfirmed):
		status, name = fiber.StatusBadRequest, "confirm_required"
	default:
		slog.Error("grainhack: request failed", "error", err)
	}
	return c.Status(status).JSON(fiber.Map{"error": name, "detail": err.Error()})
}

func (h *GrainHackPayoutHandler) adminView(c *fiber.Ctx, is *grainhack.Issued) error {
	v, err := h.svc.AdminView(c.Context(), is)
	if err != nil {
		return grainhackError(c, err)
	}
	return c.JSON(v)
}

// IssueStatement handles POST /admin/hackathons/:id/results-statement
//
//	{"confirm": true, "pool": "contributor", "payout_run_id": "<optional uuid>"}
//
// 201 with the admin view of the new statement. When one already exists, the
// same call issues the statement that supersedes it if a winner's status has
// changed, and answers 409 nothing_to_supersede otherwise.
func (h *GrainHackPayoutHandler) IssueStatement() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !h.ready() {
			return h.unconfigured(c)
		}
		hid, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		var body struct {
			Confirm     bool    `json:"confirm"`
			Pool        string  `json:"pool"`
			PayoutRunID *string `json:"payout_run_id"`
		}
		if len(c.Body()) > 0 {
			if err := c.BodyParser(&body); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
			}
		}
		req := grainhack.IssueRequest{HackathonID: hid, Pool: body.Pool, ActorID: adminActor(c), Confirm: body.Confirm}
		if body.PayoutRunID != nil && *body.PayoutRunID != "" {
			id, err := uuid.Parse(*body.PayoutRunID)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_payout_run_id"})
			}
			req.PayoutRunID = &id
		}
		is, err := h.svc.Issue(c.Context(), req)
		if err != nil {
			return grainhackError(c, err)
		}
		c.Status(fiber.StatusCreated)
		return h.adminView(c, is)
	}
}

// LatestStatement handles GET /admin/hackathons/:id/results-statement?pool=contributor
//
// The latest statement and its signature, or 404 not_found with the event's
// current computation id (null when nothing is computed yet). Reads need only
// the database, so they work without a signing key.
func (h *GrainHackPayoutHandler) LatestStatement() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unconfigured(c)
		}
		hid, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		pool := c.Query("pool", grainhack.PoolContributor)
		if pool != grainhack.PoolContributor {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "pool_unsupported"})
		}
		is, err := h.svc.Latest(c.Context(), hid, pool)
		if errors.Is(err, grainhack.ErrNotFound) {
			current, _ := grainhack.CurrentPayoutRun(c.Context(), h.svc.Pool, hid)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "not_found", "current_payout_run_id": current,
			})
		}
		if err != nil {
			return grainhackError(c, err)
		}
		return h.adminView(c, is)
	}
}

// AgentGetStatement handles GET /grainhack/results-statements/:statement_id
// (bearer GRAINHACK_STATEMENT_TOKEN). Exactly the contract's shape:
// {"statement": "<canonical json>", "signature": "<base64>"}.
func (h *GrainHackPayoutHandler) AgentGetStatement(c *fiber.Ctx) error {
	if ok, resp := h.agentAuthorized(c); !ok {
		return resp
	}
	if h.svc == nil {
		return h.unconfigured(c)
	}
	sid, err := uuid.Parse(c.Params("statement_id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_statement_id"})
	}
	is, err := h.svc.Get(c.Context(), sid)
	if err != nil {
		return grainhackError(c, err)
	}
	return agentStatement(c, is)
}

// AgentLatestStatement handles GET /grainhack/hackathons/:hackathon_id/results-statement
// (bearer), for the agent's automatic poll: the same shape as
// AgentGetStatement, for the head of the event's contributor-pool chain.
func (h *GrainHackPayoutHandler) AgentLatestStatement(c *fiber.Ctx) error {
	if ok, resp := h.agentAuthorized(c); !ok {
		return resp
	}
	if h.svc == nil {
		return h.unconfigured(c)
	}
	hid, err := uuid.Parse(c.Params("hackathon_id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
	}
	is, err := h.svc.Latest(c.Context(), hid, grainhack.PoolContributor)
	if err != nil {
		return grainhackError(c, err)
	}
	return agentStatement(c, is)
}

// agentStatement answers the agent with {statement, signature}, or 410
// statement_redacted for a statement the payout-record retention period has
// redacted: there is no signed document left to give.
func agentStatement(c *fiber.Ctx, is *grainhack.Issued) error {
	if is.RedactedAt != nil {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "statement_redacted", "redacted_at": is.RedactedAt})
	}
	return c.JSON(fiber.Map{"statement": is.Statement, "signature": is.Signature})
}

// AgentReportPayment handles POST /grainhack/payments (bearer).
//
//	{"statement_id", "github_user_id", "amount_minor", "currency", "network", "tx_signature", "recipient"}
//
// 200 {"accepted": true, "duplicate": bool, "notified": bool}.
func (h *GrainHackPayoutHandler) AgentReportPayment(c *fiber.Ctx) error {
	if ok, resp := h.agentAuthorized(c); !ok {
		return resp
	}
	if h.svc == nil {
		return h.unconfigured(c)
	}
	var r grainhack.PaymentReport
	if err := c.BodyParser(&r); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	res, err := h.svc.ReportPayment(c.Context(), r)
	if err != nil {
		return grainhackError(c, err)
	}
	return c.JSON(res)
}

// AgentAwaitingWallet handles POST /grainhack/awaiting-wallet (bearer).
//
//	{"statement_id": "<uuid>", "github_user_ids": [123, 456]}
//
// 200 {"notified": [...], "skipped": [{"github_user_id", "reason"}]}.
func (h *GrainHackPayoutHandler) AgentAwaitingWallet(c *fiber.Ctx) error {
	if ok, resp := h.agentAuthorized(c); !ok {
		return resp
	}
	if h.svc == nil {
		return h.unconfigured(c)
	}
	var r grainhack.WalletReport
	if err := c.BodyParser(&r); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	res, err := h.svc.RemindLinkWallet(c.Context(), r)
	if err != nil {
		return grainhackError(c, err)
	}
	return c.JSON(res)
}

// PublicKey handles GET /grainhack/results-key: the public half of the key
// that signs statements, so the pairing with GRAINHACK_RESULTS_PUBKEY on the
// agent and signer can be checked from outside. Public and safe: a public key
// verifies and cannot sign.
func (h *GrainHackPayoutHandler) PublicKey(c *fiber.Ctx) error {
	if !h.ready() {
		return h.unconfigured(c)
	}
	return c.JSON(fiber.Map{
		"public_key": grainhack.PublicKeyB64(h.svc.Key),
		"domain":     grainhack.SignatureDomain,
		"network":    h.svc.Network,
	})
}

// BaseSepoliaNoticePreview handles GET /admin/grainhack/base-sepolia-notice:
// who would receive the one-time Solana notice, the exact text, and whether it
// has been sent.
func (h *GrainHackPayoutHandler) BaseSepoliaNoticePreview() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unconfigured(c)
		}
		p, err := grainhack.PreviewBaseSepoliaNotice(c.Context(), h.svc.Pool)
		if err != nil {
			return grainhackError(c, err)
		}
		return c.JSON(p)
	}
}

// BaseSepoliaNoticeSend handles POST /admin/grainhack/base-sepolia-notice
// {"confirm": true}: sends it once. 409 already_sent after that.
func (h *GrainHackPayoutHandler) BaseSepoliaNoticeSend() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unconfigured(c)
		}
		var body struct {
			Confirm bool `json:"confirm"`
		}
		if len(c.Body()) > 0 {
			if err := c.BodyParser(&body); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
			}
		}
		p, err := grainhack.SendBaseSepoliaNotice(c.Context(), h.svc.Pool, adminActor(c), body.Confirm)
		if err != nil {
			return grainhackError(c, err)
		}
		return c.JSON(p)
	}
}
