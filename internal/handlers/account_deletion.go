package handlers

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/erasure"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// AccountDeletionHandler lets somebody request deletion of their own account,
// see where it stands, and cancel it during the grace period.
//
// Every route acts on the caller and only the caller: the user comes from the
// token, and no route takes a user id from the request. There is no route by
// which anybody - an administrator included - requests deletion of somebody
// else's account.
type AccountDeletionHandler struct {
	db    *db.DB
	notif *notifications.Service
	now   func() time.Time
}

func NewAccountDeletionHandler(d *db.DB, notif *notifications.Service) *AccountDeletionHandler {
	return &AccountDeletionHandler{db: d, notif: notif, now: time.Now}
}

// DeletionConfirmPhrase is what the person types to confirm. A phrase rather
// than a checkbox because the action cannot be undone once the grace period
// ends, and a checkbox is ticked by habit.
const DeletionConfirmPhrase = "delete my account"

type deletionPolicy struct {
	GraceDays int `json:"grace_days"`
	// MaxHoldDays is the longest a due deletion waits for money still on its
	// way (erasure.MaxHold), so the screen can say so before anybody confirms.
	MaxHoldDays int            `json:"max_hold_days"`
	Erased      []erasure.Item `json:"erased"`
	Retained    []erasure.Item `json:"retained"`
}

func policy() deletionPolicy {
	return deletionPolicy{
		GraceDays:   int(erasure.GracePeriod / (24 * time.Hour)),
		MaxHoldDays: int(erasure.MaxHold / (24 * time.Hour)),
		Erased:      erasure.Erased,
		Retained:    erasure.Retained,
	}
}

// Get answers GET /me/deletion: the latest request (or null), what deletion
// erases and keeps, and whether money is still on its way - so the
// confirmation screen can say "your deletion will wait for this" before the
// person confirms, not after.
func (h *AccountDeletionHandler) Get(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	req, err := erasure.Latest(c.Context(), h.db.Pool, uid)
	if err != nil {
		slog.Error("deletion: read request failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	inFlight, err := erasure.MoneyInFlight(c.Context(), h.db.Pool, uid)
	if err != nil {
		slog.Error("deletion: money-in-flight check failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "lookup_failed"})
	}
	if inFlight == nil {
		inFlight = []string{}
	}
	return c.JSON(fiber.Map{
		"request":         req,
		"policy":          policy(),
		"money_in_flight": inFlight,
		"confirm_phrase":  DeletionConfirmPhrase,
	})
}

// Request answers POST /me/deletion with {"confirm": "delete my account"}.
func (h *AccountDeletionHandler) Request(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	_ = c.BodyParser(&in)
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), DeletionConfirmPhrase) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":   "confirmation_required",
			"message": `Type "` + DeletionConfirmPhrase + `" to confirm.`,
		})
	}
	req, created, err := erasure.Schedule(c.Context(), h.db.Pool, uid, h.now())
	if errors.Is(err, erasure.ErrAlreadyErased) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "account_erased"})
	}
	if err != nil {
		slog.Error("deletion: schedule failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "request_failed"})
	}
	if created {
		slog.Info("deletion: requested", "user_id", uid, "request_id", req.ID, "execute_after", req.ExecuteAfter)
		h.notif.Notify(c.Context(), uid, notifications.TypeAccountDeletion,
			"Your account is scheduled for deletion",
			"You asked us to delete your Grainlify account. It will be erased on "+
				req.ExecuteAfter.UTC().Format("2 January 2006")+
				". Until then you can cancel in Settings. If you did not ask for this, cancel it now.",
			notifications.SettingsLink(notifications.SubtabAccount))
	}
	status := fiber.StatusOK
	if created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(fiber.Map{"request": req, "created": created})
}

// Cancel answers POST /me/deletion/cancel.
func (h *AccountDeletionHandler) Cancel(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	req, err := erasure.Cancel(c.Context(), h.db.Pool, uid, h.now())
	if errors.Is(err, erasure.ErrNoOpenRequest) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":   "nothing_to_cancel",
			"message": "There is no deletion request waiting to be carried out.",
		})
	}
	if err != nil {
		slog.Error("deletion: cancel failed", "user_id", uid, "error", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "cancel_failed"})
	}
	slog.Info("deletion: cancelled", "user_id", uid, "request_id", req.ID)
	h.notif.Notify(c.Context(), uid, notifications.TypeAccountDeletion,
		"Account deletion cancelled",
		"Your account deletion request was cancelled. Nothing was deleted.",
		notifications.SettingsLink(notifications.SubtabAccount))
	return c.JSON(fiber.Map{"request": req})
}

// RefuseErasedAccounts ends access for an erased account immediately.
//
// Sign-in tokens are stateless and live 60 minutes, so a token issued before
// an erasure would otherwise keep working for up to an hour after it - long
// enough to write new personal data onto the tombstone through the profile
// endpoints. This runs before every route and refuses any bearer token whose
// account has been erased (or no longer exists).
//
// Requests without a bearer token, or with one that does not parse, pass
// through untouched: RequireAuth answers those, and public routes do not care.
// A database error refuses with 503, not 401, so a blip does not sign
// everybody out.
func RefuseErasedAccounts(jwtSecret string, d *db.DB) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if d == nil || d.Pool == nil {
			return c.Next()
		}
		h := strings.TrimSpace(c.Get("Authorization"))
		if len(h) < len("bearer ") || !strings.EqualFold(h[:len("bearer ")], "bearer ") {
			return c.Next()
		}
		claims, err := auth.ParseJWT(jwtSecret, strings.TrimSpace(h[len("bearer "):]))
		if err != nil {
			return c.Next()
		}
		uid, err := uuid.Parse(claims.Subject)
		if err != nil {
			return c.Next()
		}
		var erased bool
		err = d.Pool.QueryRow(c.Context(), `SELECT erased_at IS NOT NULL FROM users WHERE id = $1`, uid).Scan(&erased)
		// No row at all is left to the routes, as it always was: erasure never
		// deletes the row, so a missing one is not an erased one.
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Next()
		}
		if err != nil {
			slog.Error("erased-account check failed; refusing", "path", c.Path(), "error", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "account_check_unavailable"})
		}
		if erased {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error":   "account_erased",
				"message": "This account has been deleted.",
			})
		}
		return c.Next()
	}
}
