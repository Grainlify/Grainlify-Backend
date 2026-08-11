package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// appWith mounts RequireLiveRole behind a stub that injects the given locals,
// so every branch is reachable without a database or a real token.
func appWith(lookup RoleLookupFunc, userID, claimedRole string) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		if userID != "" {
			c.Locals(LocalUserID, userID)
		}
		if claimedRole != "" {
			c.Locals(LocalRole, claimedRole)
		}
		return c.Next()
	})
	app.Get("/admin/thing", RequireLiveRole(lookup, "admin"), func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app
}

func status(t *testing.T, app *fiber.App) int {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/admin/thing", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	return resp.StatusCode
}

// TestRequireLiveRole_FailsClosedOnLookupError is the rule that matters most:
// "we could not check the role" must never read as "the role is admin". A
// database blip is a reason to refuse an admin action, not a reason to fall
// back to an unverified claim.
func TestRequireLiveRole_FailsClosedOnLookupError(t *testing.T) {
	id := uuid.NewString()
	failing := func(context.Context, uuid.UUID) (string, error) {
		return "", errors.New("connection refused")
	}
	// The JWT says admin. It must not matter.
	if got := status(t, appWith(failing, id, "admin")); got != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d when the role lookup fails", got, fiber.StatusServiceUnavailable)
	}
}

// TestRequireLiveRole_FailsClosedWhenUnconfigured: a misconfigured server
// must not authorise anybody.
func TestRequireLiveRole_FailsClosedWhenUnconfigured(t *testing.T) {
	if got := status(t, appWith(nil, uuid.NewString(), "admin")); got != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d with no lookup configured", got, fiber.StatusServiceUnavailable)
	}
}

// TestRequireLiveRole_RejectsWhenClaimDisagreesWithDatabase. A stale token is
// the revocation case: the claim says admin, the database says otherwise.
// Rejecting rather than resolving it either way is deliberate - silently
// using the live role would let a stale token keep working under a role its
// holder was never issued.
func TestRequireLiveRole_RejectsWhenClaimDisagreesWithDatabase(t *testing.T) {
	id := uuid.NewString()
	demoted := func(context.Context, uuid.UUID) (string, error) { return "contributor", nil }
	if got := status(t, appWith(demoted, id, "admin")); got != fiber.StatusForbidden {
		t.Errorf("status = %d, want %d for a token claiming admin after demotion", got, fiber.StatusForbidden)
	}

	// The reverse disagreement is refused too: a token issued before a
	// promotion does not become an admin token retroactively.
	promoted := func(context.Context, uuid.UUID) (string, error) { return "admin", nil }
	if got := status(t, appWith(promoted, id, "contributor")); got != fiber.StatusForbidden {
		t.Errorf("status = %d, want %d for a stale pre-promotion token", got, fiber.StatusForbidden)
	}
}

// TestRequireLiveRole_RevocationIsImmediate is the whole point: no caching,
// so the very next request after a demotion is refused.
func TestRequireLiveRole_RevocationIsImmediate(t *testing.T) {
	id := uuid.NewString()
	role := "admin"
	lookup := func(context.Context, uuid.UUID) (string, error) { return role, nil }
	app := appWith(lookup, id, "admin")

	if got := status(t, app); got != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 while still an admin", got)
	}
	// Demote. No new token, no expiry wait, no cache to clear.
	role = "contributor"
	if got := status(t, app); got == fiber.StatusOK {
		t.Error("still authorised on the request immediately after demotion")
	}
}

// TestRequireLiveRole_AllowsAMatchingAdmin - the guard has to let the real
// case through, or it is just an outage.
func TestRequireLiveRole_AllowsAMatchingAdmin(t *testing.T) {
	lookup := func(context.Context, uuid.UUID) (string, error) { return "admin", nil }
	if got := status(t, appWith(lookup, uuid.NewString(), "admin")); got != fiber.StatusOK {
		t.Errorf("status = %d, want 200 for a genuine admin", got)
	}
}

// TestRequireLiveRole_RefusesNonAdminEvenWithAgreeingClaim.
func TestRequireLiveRole_RefusesNonAdminEvenWithAgreeingClaim(t *testing.T) {
	lookup := func(context.Context, uuid.UUID) (string, error) { return "contributor", nil }
	if got := status(t, appWith(lookup, uuid.NewString(), "contributor")); got != fiber.StatusForbidden {
		t.Errorf("status = %d, want %d for a contributor", got, fiber.StatusForbidden)
	}
}
