package auth

import (
	"context"
	"log/slog"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// RoleLookupFunc reads a user's CURRENT role from the source of truth.
//
// A function rather than a database handle so this package stays free of a
// db dependency, and so a test can drive every branch below without a
// database.
type RoleLookupFunc func(ctx context.Context, userID uuid.UUID) (string, error)

// RequireLiveRole authorises against the role in the database, not the role
// baked into the caller's JWT.
//
// The JWT carries a role claim and lives 60 minutes. Authorising on that claim
// means a revoked admin keeps admin for up to an hour after the decision to
// remove them - there is no revocation, only expiry. For endpoints that set
// draw weights, pool sizes and wave boundaries, "eventually not an admin" is
// not a useful guarantee.
//
// **Nothing here is cached, deliberately.** A lookup cache would make
// revocation eventual again, which is the property this exists to remove.
// Admin traffic is a handful of requests; one indexed primary-key read per
// request is not a cost worth trading that for. If someone adds a cache later
// for performance, they have quietly reverted this change.
//
// Every failure path refuses. "We could not check the role" must never read as
// "the role is admin" - the same rule the escrow verification follows, and for
// the same reason: an unverified claim is not a verified one.
func RequireLiveRole(lookup RoleLookupFunc, roles ...string) fiber.Handler {
	allowed := map[string]struct{}{}
	for _, r := range roles {
		allowed[r] = struct{}{}
	}

	return func(c *fiber.Ctx) error {
		idStr, _ := c.Locals(LocalUserID).(string)
		userID, err := uuid.Parse(idStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		claimed, _ := c.Locals(LocalRole).(string)

		if lookup == nil {
			// A misconfigured server must not authorise anybody.
			slog.Error("auth: live role lookup not configured; refusing", "path", c.Path())
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "role_check_unavailable"})
		}

		live, err := lookup(c.Context(), userID)
		if err != nil {
			// Fail closed. A database blip is a reason to refuse an admin
			// action, not a reason to trust an unverified claim.
			slog.Error("auth: live role lookup failed; refusing",
				"path", c.Path(), "user_id", userID, "error", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "role_check_unavailable"})
		}

		// A claim that disagrees with the database is rejected rather than
		// resolved in either direction. It means the token predates a role
		// change - which is exactly the revocation case - and silently using
		// the live role would let a stale token keep working under a role its
		// holder was never issued.
		if claimed != "" && claimed != live {
			slog.Warn("auth: token role disagrees with stored role; refusing",
				"path", c.Path(), "user_id", userID, "claimed_role", claimed, "live_role", live)
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":   "role_changed",
				"message": "Your access level changed. Sign in again.",
			})
		}

		if _, ok := allowed[live]; !ok {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "insufficient_role"})
		}
		return c.Next()
	}
}
