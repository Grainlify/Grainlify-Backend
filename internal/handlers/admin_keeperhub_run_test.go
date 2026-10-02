package handlers_test

import (
	"context"
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

// The id release needs, in both answers the screen can get: the 404 for an
// event with no run yet (the first payout, which is when it is needed) and the
// 200 once a run exists. Both fields are always present; both are null only
// when nothing has been computed.
func TestKeeperHubRunRead_CarriesTheLatestPayoutRunID(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	h := handlers.NewAdminKeeperHubPayoutHandler(d, config.Config{})
	app := keeperhubReadApp(t, h)
	admin := adminSuiteInsertUser(t, d, "admin")
	tok := adminSuiteToken(t, admin, "admin")

	// Nothing computed: 404 not_found, with both fields present and null.
	code, body := adminSuiteDo(t, app, "GET", "/admin/hackathons/"+uuid.NewString()+"/keeperhub/run", tok, nil)
	if code != fiber.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("got %d %v, want 404 not_found", code, body)
	}
	for _, k := range []string{"latest_payout_run_id", "latest_payout_run_created_at"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Fatalf("%s = %v (present %v), want present and null with nothing computed", k, v, ok)
		}
	}

	var hid, older, newest uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name) VALUES ($1) RETURNING id`,
		"kh-read-"+uuid.NewString()[:8]).Scan(&hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	chain := "khread-evm-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM hackathons WHERE id = $1`, hid)
		d.Pool.Exec(context.Background(), `DELETE FROM chain_configs WHERE chain_id = $1`, chain)
	})
	computation := func(at string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := d.Pool.QueryRow(ctx, `
			INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value, created_at)
			VALUES ($1, 7, 7, 1, `+at+`) RETURNING id`, hid).Scan(&id); err != nil {
			t.Fatalf("payout run: %v", err)
		}
		return id
	}
	older = computation("now() - interval '1 hour'")
	newest = computation("now()")
	path := "/admin/hackathons/" + hid.String() + "/keeperhub/run?pool=contributor"

	// Computed, no run yet: still 404 not_found - a client that reads that as
	// "no run" keeps working - and now carrying the newest computation.
	code, body = adminSuiteDo(t, app, "GET", path, tok, nil)
	if code != fiber.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("got %d %v, want 404 not_found", code, body)
	}
	if body["latest_payout_run_id"] != newest.String() {
		t.Fatalf("latest_payout_run_id = %v, want the newest computation %s (not %s)", body["latest_payout_run_id"], newest, older)
	}
	if s, _ := body["latest_payout_run_created_at"].(string); s == "" {
		t.Fatalf("latest_payout_run_created_at = %v, want a timestamp", body["latest_payout_run_created_at"])
	}

	// A run planned from the older computation: 200, the run still names the
	// computation it was planned from, and the top level names the newest.
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO chain_configs (chain_id, family, enabled, asset, min_confirmations, evm_chain_id)
		VALUES ($1, 'evm', true, '{"symbol":"USDC","decimals":6}'::jsonb, 1, $2)`,
		chain, 900_000_000_000+int64(uuid.New().ID())); err != nil {
		t.Fatalf("chain: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id, released_by, state)
		SELECT $1, 'contributor', $2, evm_chain_id, 7000000, $3, $4, 'planned' FROM chain_configs WHERE chain_id = $2`,
		hid, chain, older, admin); err != nil {
		t.Fatalf("keeperhub run: %v", err)
	}
	code, body = adminSuiteDo(t, app, "GET", path, tok, nil)
	if code != fiber.StatusOK {
		t.Fatalf("got %d %v, want 200", code, body)
	}
	run, _ := body["run"].(map[string]any)
	if run["payout_run_id"] != older.String() {
		t.Fatalf("run.payout_run_id = %v, want the computation it was planned from %s", run["payout_run_id"], older)
	}
	if body["latest_payout_run_id"] != newest.String() {
		t.Fatalf("latest_payout_run_id = %v, want %s at the top level", body["latest_payout_run_id"], newest)
	}
	if s, _ := body["latest_payout_run_created_at"].(string); s == "" {
		t.Fatalf("latest_payout_run_created_at = %v, want a timestamp", body["latest_payout_run_created_at"])
	}
	if _, nested := run["latest_payout_run_id"]; nested {
		t.Fatal("latest_payout_run_id is inside run; it describes the event and belongs at the top level")
	}
}
