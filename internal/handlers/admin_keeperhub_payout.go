package handlers

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhubrail"
)

// AdminKeeperHubPayoutHandler releases a settled event's contributor pool over
// KeeperHub and reads the results back.
//
// Thin on purpose: every rule lives in internal/keeperhubrail, where it can be
// tested against a database without a web server, and this file only maps
// requests in and refusals out.
type AdminKeeperHubPayoutHandler struct {
	svc       *keeperhubrail.Service
	configErr error

	// reader serves the GET. It is always present, because reading state
	// needs only this database: an unconfigured rail must not hide legs that
	// still need reconciling.
	reader *keeperhubrail.Service
}

// NewAdminKeeperHubPayoutHandler builds the client only when all three KeeperHub
// values are configured, mirroring the Didit pattern. Unconfigured, every
// endpoint answers 503 naming what is missing, rather than failing at the moment
// somebody tries to pay.
func NewAdminKeeperHubPayoutHandler(d *db.DB, cfg config.Config) *AdminKeeperHubPayoutHandler {
	// The API is wired without a database in some tests and tooling. Every
	// route then answers 503 rather than dereferencing nothing at startup.
	if d == nil || d.Pool == nil {
		return &AdminKeeperHubPayoutHandler{configErr: errors.New("database not configured")}
	}
	reader := &keeperhubrail.Service{Pool: d.Pool}
	c, err := keeperhub.New(cfg.KeeperHubWebhookKey, cfg.KeeperHubAPIKey, cfg.KeeperHubWorkflowID)
	if err != nil {
		return &AdminKeeperHubPayoutHandler{configErr: err, reader: reader}
	}
	return &AdminKeeperHubPayoutHandler{svc: &keeperhubrail.Service{Pool: d.Pool, Rail: c}, reader: reader}
}

// NewAdminKeeperHubPayoutHandlerWith injects a service, for tests.
func NewAdminKeeperHubPayoutHandlerWith(svc *keeperhubrail.Service) *AdminKeeperHubPayoutHandler {
	return &AdminKeeperHubPayoutHandler{svc: svc, reader: svc}
}

// Run handles GET /admin/hackathons/:id/keeperhub/run?pool=contributor
//
// Everything the payout admin screen shows for one event and pool: the run,
// per-status figures derived from the legs, what a release would do now, every
// leg, every dispatch attempt and every exclusion. 404 not_found when the event
// has no run for the pool.
//
// # No 503 here, deliberately
//
// The write routes answer 503 when KeeperHub is not configured, because they
// cannot act. This one calls nothing outside the database, and the state it
// shows matters most exactly when the rail is down: legs that may have paid
// still need reconciling. So it always reads, and the resume summary carries
// keeperhub_not_configured as the reason nothing can be sent.
func (h *AdminKeeperHubPayoutHandler) Run() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.reader == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "database_not_configured"})
		}
		hid, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		pool := c.Query("pool", keeperhubrail.PoolContributor)
		view, err := h.reader.RunView(c.Context(), hid, pool, adminActor(c), h.configErr)
		if err != nil {
			return keeperhubError(c, err, hid)
		}
		return c.JSON(view)
	}
}

func (h *AdminKeeperHubPayoutHandler) unavailable(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"error":  "keeperhub_not_configured",
		"detail": h.configErr.Error(),
	})
}

func adminActor(c *fiber.Ctx) uuid.UUID {
	s, _ := c.Locals(auth.LocalUserID).(string)
	id, _ := uuid.Parse(s)
	return id
}

// Release handles POST /admin/hackathons/:id/keeperhub/release
//
//	{"payout_run_id": "...", "chain_id": "...", "pool": "contributor", "confirm": true}
//
// Answers 202, never 200: the run was ACCEPTED by KeeperHub, which is not the
// same as anybody being paid. The response says so, and names the intake call
// that establishes what actually happened.
func (h *AdminKeeperHubPayoutHandler) Release() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unavailable(c)
		}
		hid, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}
		var body struct {
			PayoutRunID string `json:"payout_run_id"`
			ChainID     string `json:"chain_id"`
			Pool        string `json:"pool"`
			Confirm     bool   `json:"confirm"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
		}
		runID, err := uuid.Parse(body.PayoutRunID)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_payout_run_id"})
		}
		if body.Pool == "" {
			body.Pool = keeperhubrail.PoolContributor
		}

		res, err := h.svc.Release(c.Context(), keeperhubrail.ReleaseRequest{
			HackathonID: hid,
			PayoutRunID: runID,
			ActorID:     adminActor(c),
			Pool:        body.Pool,
			ChainID:     body.ChainID,
			Confirm:     body.Confirm,
		})
		if err != nil {
			return keeperhubError(c, err, hid)
		}
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"release": res,
			"note": "KeeperHub accepted the run. This is not a payment: no leg is confirmed until " +
				"its result is read back with POST .../keeperhub/attempts/" + res.AttemptID.String() + "/intake.",
		})
	}
}

// Intake handles POST /admin/hackathons/:id/keeperhub/attempts/:attempt_id/intake
func (h *AdminKeeperHubPayoutHandler) Intake() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unavailable(c)
		}
		hid, err1 := uuid.Parse(c.Params("id"))
		aid, err2 := uuid.Parse(c.Params("attempt_id"))
		if err1 != nil || err2 != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_id"})
		}
		res, err := h.svc.Intake(c.Context(), hid, aid)
		var missing *keeperhub.MissingResultsError
		switch {
		case err == nil:
			return c.JSON(fiber.Map{"intake": res})
		case errors.As(err, &missing):
			// Written, but not clean: these legs now block every resume.
			return c.JSON(fiber.Map{
				"intake":   res,
				"blocking": true,
				"detail":   err.Error(),
			})
		case errors.Is(err, keeperhub.ErrExecutionInputMismatch):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error":  "execution_input_mismatch",
				"intake": res,
				"detail": err.Error(),
			})
		default:
			return keeperhubError(c, err, hid)
		}
	}
}

// Resolve handles POST /admin/hackathons/:id/keeperhub/legs/:leg_id/resolve
//
//	{"status": "confirmed"|"failed", "tx_hash": "0x...", "note": "what was checked on chain"}
func (h *AdminKeeperHubPayoutHandler) Resolve() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.svc == nil {
			return h.unavailable(c)
		}
		hid, err1 := uuid.Parse(c.Params("id"))
		lid, err2 := uuid.Parse(c.Params("leg_id"))
		if err1 != nil || err2 != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_id"})
		}
		var body struct {
			Status string `json:"status"`
			TxHash string `json:"tx_hash"`
			Note   string `json:"note"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
		}
		if err := h.svc.ResolveLeg(c.Context(), keeperhubrail.ResolveRequest{
			HackathonID: hid, LegID: lid, ActorID: adminActor(c),
			Status: body.Status, TxHash: body.TxHash, Note: body.Note,
		}); err != nil {
			return keeperhubError(c, err, hid)
		}
		return c.JSON(fiber.Map{"leg_id": lid, "status": body.Status})
	}
}

// keeperhubError maps one refusal to one name.
func keeperhubError(c *fiber.Ctx, err error, hid uuid.UUID) error {
	status, name := fiber.StatusInternalServerError, "keeperhub_payout_failed"
	var unreconciled *keeperhubrail.UnreconciledLegsError
	switch {
	case errors.Is(err, hackathon.ErrPayoutNotReleasable):
		status, name = fiber.StatusConflict, "payout_not_releasable"
	case errors.As(err, &unreconciled):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":   "unreconciled_legs",
			"leg_ids": unreconciled.LegIDs,
			"detail":  err.Error(),
		})
	case errors.Is(err, keeperhubrail.ErrSettledOnAptos):
		status, name = fiber.StatusConflict, "settled_on_aptos_rail"
	case errors.Is(err, keeperhubrail.ErrNothingUnpaid):
		status, name = fiber.StatusConflict, "nothing_unpaid"
	case errors.Is(err, keeperhubrail.ErrRunMismatch):
		status, name = fiber.StatusConflict, "run_mismatch"
	case errors.Is(err, keeperhubrail.ErrPayoutRunNotCurrent):
		status, name = fiber.StatusConflict, "payout_run_not_current"
	case errors.Is(err, keeperhubrail.ErrConcurrentRelease):
		status, name = fiber.StatusConflict, "concurrent_release"
	case errors.Is(err, keeperhubrail.ErrNotEVMChain):
		status, name = fiber.StatusBadRequest, "chain_not_evm"
	case errors.Is(err, keeperhubrail.ErrUnsupportedPool):
		status, name = fiber.StatusBadRequest, "pool_unsupported"
	case errors.Is(err, keeperhubrail.ErrResolutionIncomplete):
		status, name = fiber.StatusBadRequest, "resolution_incomplete"
	case errors.Is(err, keeperhubrail.ErrLegNotResolvable):
		status, name = fiber.StatusConflict, "leg_not_resolvable"
	case errors.Is(err, keeperhubrail.ErrNoExecutionToRead):
		status, name = fiber.StatusConflict, "no_execution_to_read"
	case errors.Is(err, keeperhubrail.ErrNotFound):
		status, name = fiber.StatusNotFound, "not_found"
	case errors.Is(err, hackathon.ErrNothingToSettle):
		status, name = fiber.StatusConflict, "nothing_to_settle"
	case errors.Is(err, keeperhubrail.ErrChainMismatch):
		slog.Error("keeperhub chain mismatch", "hackathon_id", hid, "error", err)
		status, name = fiber.StatusConflict, "chain_mismatch"
	case errors.Is(err, keeperhubrail.ErrRunFailed):
		status, name = fiber.StatusConflict, "run_failed"
	case errors.Is(err, keeperhubrail.ErrDispatchRejected):
		// Certain: nothing ran. The legs are failed and the next release may
		// send them, once whatever KeeperHub refused (a key, a disabled
		// workflow, a PAYG block) is fixed.
		slog.Warn("keeperhub dispatch rejected", "hackathon_id", hid, "error", err)
		status, name = fiber.StatusBadGateway, "dispatch_rejected"
	case errors.Is(err, keeperhubrail.ErrDispatchUnknown):
		// Upstream trouble, and the legs are now unknown. Logged at error:
		// money may be moving under a request we could not confirm.
		slog.Error("keeperhub dispatch outcome unknown", "hackathon_id", hid, "error", err)
		status, name = fiber.StatusBadGateway, "dispatch_outcome_unknown"
	default:
		slog.Error("keeperhub payout", "hackathon_id", hid, "error", err)
	}
	return c.Status(status).JSON(fiber.Map{"error": name, "detail": err.Error()})
}
