package handlers_test

import (
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ecosystemsAdminSuiteApp mounts /admin/ecosystems... exactly as
// internal/api/api.go wires EcosystemsAdminHandler: RequireAuth +
// RequireRole admin on every route. Reuses adminSuiteJWTSecret /
// adminSuiteInsertUser / adminSuiteToken / adminSuiteDo from admin_test.go.
func ecosystemsAdminSuiteApp(d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewEcosystemsAdminHandler(d)
	adminGroup := app.Group("/admin", auth.RequireAuth(adminSuiteJWTSecret))
	adminGroup.Get("/ecosystems", auth.RequireRole("admin"), h.List())
	adminGroup.Get("/ecosystems/:id", auth.RequireRole("admin"), h.GetByID())
	adminGroup.Post("/ecosystems", auth.RequireRole("admin"), h.Create())
	adminGroup.Put("/ecosystems/:id", auth.RequireRole("admin"), h.Update())
	adminGroup.Delete("/ecosystems/:id", auth.RequireRole("admin"), h.Delete())
	return app
}

// ecosystemsAdminSuiteCreate is a small convenience wrapper that creates an
// ecosystem through the handler itself (not raw SQL) and returns its id,
// used by tests that need a fixture row but aren't testing Create directly.
func ecosystemsAdminSuiteCreate(t *testing.T, app *fiber.App, adminToken, name string) string {
	t.Helper()
	status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", adminToken, map[string]string{"name": name})
	if status != fiber.StatusCreated {
		t.Fatalf("fixture create: status = %d, want %d (body=%v)", status, fiber.StatusCreated, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("fixture create: expected non-empty id in response, got %v", body)
	}
	return id
}

func TestEcosystemsAdminList_RBAC(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "GET", "/admin/ecosystems", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")
		status, _ := adminSuiteDo(t, app, "GET", "/admin/ecosystems", tok, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("authenticated admin succeeds", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")
		status, body := adminSuiteDo(t, app, "GET", "/admin/ecosystems", tok, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		if _, ok := body["ecosystems"]; !ok {
			t.Errorf("expected \"ecosystems\" key in response, got %v", body)
		}
	})
}

func TestEcosystemsAdminCreate_RBAC(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)
	validName := "RBAC Ecosystem " + uuid.NewString()

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "POST", "/admin/ecosystems", "", map[string]string{"name": validName})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "maintainer")
		tok := adminSuiteToken(t, uid, "maintainer")
		status, _ := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{"name": validName})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("authenticated admin succeeds", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")
		status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{"name": "Admin Create Eco " + uuid.NewString()})
		if status != fiber.StatusCreated {
			t.Fatalf("status = %d, want %d (body=%v)", status, fiber.StatusCreated, body)
		}
		if id, _ := body["id"].(string); id == "" {
			t.Errorf("expected non-empty id in response, got %v", body)
		}
	})
}

func TestEcosystemsAdminCreate_Validation(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)
	uid := adminSuiteInsertUser(t, d, "admin")
	tok := adminSuiteToken(t, uid, "admin")

	t.Run("missing name is rejected", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "name_required" {
			t.Errorf("error = %v, want name_required", body["error"])
		}
	})

	t.Run("blank name is rejected", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{"name": "   "})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "name_required" {
			t.Errorf("error = %v, want name_required", body["error"])
		}
	})

	t.Run("name with no sluggable characters is rejected", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{"name": "!!!???"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "name_must_contain_valid_characters" {
			t.Errorf("error = %v, want name_must_contain_valid_characters", body["error"])
		}
	})

	t.Run("invalid status is rejected", func(t *testing.T) {
		status, body := adminSuiteDo(t, app, "POST", "/admin/ecosystems", tok, map[string]string{
			"name":   "Status Validation Eco " + uuid.NewString(),
			"status": "bogus",
		})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_status" {
			t.Errorf("error = %v, want invalid_status", body["error"])
		}
	})
}

func TestEcosystemsAdminGetByID_NotFound(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)
	uid := adminSuiteInsertUser(t, d, "admin")
	tok := adminSuiteToken(t, uid, "admin")

	status, body := adminSuiteDo(t, app, "GET", "/admin/ecosystems/"+uuid.NewString(), tok, nil)
	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
	}
	if body["error"] != "ecosystem_not_found" {
		t.Errorf("error = %v, want ecosystem_not_found", body["error"])
	}
}

func TestEcosystemsAdminUpdate_Persists(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)
	uid := adminSuiteInsertUser(t, d, "admin")
	tok := adminSuiteToken(t, uid, "admin")

	beforeName := "Eco Before " + uuid.NewString()
	id := ecosystemsAdminSuiteCreate(t, app, tok, beforeName)

	// Sanity-check the freshly created row before mutating it.
	status, getBody := adminSuiteDo(t, app, "GET", "/admin/ecosystems/"+id, tok, nil)
	if status != fiber.StatusOK {
		t.Fatalf("pre-update GET status = %d, want %d", status, fiber.StatusOK)
	}
	if getBody["name"] != beforeName {
		t.Fatalf("pre-update name = %v, want %v", getBody["name"], beforeName)
	}
	if getBody["status"] != "active" {
		t.Fatalf("pre-update status = %v, want active (default)", getBody["status"])
	}
	slugBefore, _ := getBody["slug"].(string)
	if slugBefore == "" {
		t.Fatalf("pre-update slug is empty, want an auto-generated slug")
	}

	afterName := "Eco After " + uuid.NewString()
	updateStatus, updateBody := adminSuiteDo(t, app, "PUT", "/admin/ecosystems/"+id, tok, map[string]string{
		"name":        afterName,
		"status":      "inactive",
		"about":       "updated about text",
		"website_url": "https://example.com/after",
	})
	if updateStatus != fiber.StatusOK {
		t.Fatalf("update status = %d, want %d (body=%v)", updateStatus, fiber.StatusOK, updateBody)
	}
	if updateBody["ok"] != true {
		t.Errorf("update ok = %v, want true", updateBody["ok"])
	}

	status, afterBody := adminSuiteDo(t, app, "GET", "/admin/ecosystems/"+id, tok, nil)
	if status != fiber.StatusOK {
		t.Fatalf("post-update GET status = %d, want %d", status, fiber.StatusOK)
	}
	if afterBody["name"] != afterName {
		t.Errorf("post-update name = %v, want %v", afterBody["name"], afterName)
	}
	if afterBody["status"] != "inactive" {
		t.Errorf("post-update status = %v, want inactive", afterBody["status"])
	}
	if afterBody["about"] != "updated about text" {
		t.Errorf("post-update about = %v, want %q", afterBody["about"], "updated about text")
	}
	if afterBody["website_url"] != "https://example.com/after" {
		t.Errorf("post-update website_url = %v, want %q", afterBody["website_url"], "https://example.com/after")
	}
	slugAfter, _ := afterBody["slug"].(string)
	if slugAfter == "" {
		t.Errorf("post-update slug is empty, want a re-derived slug")
	}
	if slugAfter == slugBefore {
		t.Errorf("slug did not change after renaming (before=%q after=%q)", slugBefore, slugAfter)
	}
}

func TestEcosystemsAdminDelete(t *testing.T) {
	d := testDB(t)
	app := ecosystemsAdminSuiteApp(d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "DELETE", "/admin/ecosystems/"+uuid.NewString(), "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")
		status, _ := adminSuiteDo(t, app, "DELETE", "/admin/ecosystems/"+uuid.NewString(), tok, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("admin deletes an existing ecosystem and it 404s afterward", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		id := ecosystemsAdminSuiteCreate(t, app, adminTok, "Eco ToDelete "+uuid.NewString())

		delStatus, delBody := adminSuiteDo(t, app, "DELETE", "/admin/ecosystems/"+id, adminTok, nil)
		if delStatus != fiber.StatusOK {
			t.Fatalf("delete status = %d, want %d (body=%v)", delStatus, fiber.StatusOK, delBody)
		}
		if delBody["ok"] != true {
			t.Errorf("delete ok = %v, want true", delBody["ok"])
		}

		getStatus, getBody := adminSuiteDo(t, app, "GET", "/admin/ecosystems/"+id, adminTok, nil)
		if getStatus != fiber.StatusNotFound {
			t.Errorf("post-delete GET status = %d, want %d", getStatus, fiber.StatusNotFound)
		}
		if getBody["error"] != "ecosystem_not_found" {
			t.Errorf("post-delete GET error = %v, want ecosystem_not_found", getBody["error"])
		}
	})

	t.Run("admin gets 404 deleting a nonexistent ecosystem", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		status, body := adminSuiteDo(t, app, "DELETE", "/admin/ecosystems/"+uuid.NewString(), adminTok, nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "ecosystem_not_found" {
			t.Errorf("error = %v, want ecosystem_not_found", body["error"])
		}
	})
}
