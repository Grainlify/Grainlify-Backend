package handlers_test

import (
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// The read goes through the same chain as every admin route: JWT, then the
// live role re-read from the database.
func keeperhubReadApp(t *testing.T, h *handlers.AdminKeeperHubPayoutHandler) *fiber.App {
	t.Helper()
	d := dbtest.DB(t)
	app := fiber.New()
	requireAdmin := auth.RequireLiveRole(handlers.NewRoleLookup(d), "admin")
	adminGroup := app.Group("/admin", auth.RequireAuth(adminSuiteJWTSecret))
	adminGroup.Get("/hackathons/:id/keeperhub/run", requireAdmin, h.Run())
	return app
}

func TestKeeperHubRunRead_RefusesANonAdmin(t *testing.T) {
	d := dbtest.DB(t)
	h := handlers.NewAdminKeeperHubPayoutHandler(d, config.Config{})
	app := keeperhubReadApp(t, h)

	contributor := adminSuiteInsertUser(t, d, "contributor")
	path := "/admin/hackathons/" + uuid.NewString() + "/keeperhub/run"

	code, body := adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, contributor, "contributor"), nil)
	if code != fiber.StatusForbidden {
		t.Fatalf("non-admin got %d %v, want 403", code, body)
	}

	// A token CLAIMING admin for a user whose live role is not admin is refused
	// too: the check reads the database, not the claim.
	code, body = adminSuiteDo(t, app, "GET", path, adminSuiteToken(t, contributor, "admin"), nil)
	if code != fiber.StatusForbidden {
		t.Fatalf("stale admin claim got %d %v, want 403", code, body)
	}

	if code, _ := adminSuiteDo(t, app, "GET", path, "", nil); code != fiber.StatusUnauthorized {
		t.Fatalf("no token got %d, want 401", code)
	}
}

// An admin reading an event with no run gets 404 not_found - and, with the rail
// unconfigured, NOT the write routes' 503: reading needs no KeeperHub call.
func TestKeeperHubRunRead_NoRunIsNotFoundEvenWithoutKeeperHub(t *testing.T) {
	d := dbtest.DB(t)
	h := handlers.NewAdminKeeperHubPayoutHandler(d, config.Config{}) // no KEEPERHUB_* at all
	app := keeperhubReadApp(t, h)

	admin := adminSuiteInsertUser(t, d, "admin")
	code, body := adminSuiteDo(t, app, "GET", "/admin/hackathons/"+uuid.NewString()+"/keeperhub/run",
		adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("got %d %v, want 404 not_found", code, body)
	}

	code, body = adminSuiteDo(t, app, "GET", "/admin/hackathons/"+uuid.NewString()+"/keeperhub/run?pool=maintainer",
		adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusBadRequest || body["error"] != "pool_unsupported" {
		t.Fatalf("maintainer pool got %d %v, want 400 pool_unsupported, as release answers", code, body)
	}

	code, body = adminSuiteDo(t, app, "GET", "/admin/hackathons/not-a-uuid/keeperhub/run",
		adminSuiteToken(t, admin, "admin"), nil)
	if code != fiber.StatusBadRequest {
		t.Fatalf("bad id got %d %v, want 400", code, body)
	}
}
