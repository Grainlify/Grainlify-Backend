package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Shared helpers for the "admin" domain test suite (admin_test.go,
// admin_ecosystems_test.go, open_source_week_test.go). Everything here is
// prefixed with adminSuite to stay unique across the concurrently-written
// test files in this package (other agents own auth-core, oauth/github-app,
// projects/issues, webhooks).
// ---------------------------------------------------------------------------

// adminSuiteJWTSecret is the fixed JWT secret used to sign/verify tokens for
// every test app built in this suite.
const adminSuiteJWTSecret = "admin-suite-test-jwt-secret"

// adminSuiteGHIDSeq guarantees unique github_user_id values even when tests
// insert many users in rapid succession within the same process.
var adminSuiteGHIDSeq int64

// adminSuiteConfig builds a config.Config for the admin test suite with the
// fixed JWT secret and the given bootstrap token.
func adminSuiteConfig(bootstrapToken string) config.Config {
	return config.Config{
		JWTSecret:           adminSuiteJWTSecret,
		AdminBootstrapToken: bootstrapToken,
	}
}

// adminSuiteInsertUser inserts a uniquely-identified user row with the given
// role directly via SQL and returns its id.
func adminSuiteInsertUser(t *testing.T, d *db.DB, role string) uuid.UUID {
	t.Helper()
	seq := atomic.AddInt64(&adminSuiteGHIDSeq, 1)
	ghID := time.Now().UnixNano() + seq

	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ($1, $2, $3)
RETURNING id
`, role, "admin-suite-user-"+uuid.NewString(), ghID).Scan(&id)
	if err != nil {
		t.Fatalf("insert admin-suite test user: %v", err)
	}
	return id
}

// adminSuiteToken issues a signed JWT for userID/role using the admin
// suite's fixed JWT secret, matching how production issues tokens.
func adminSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(adminSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("issue admin-suite jwt: %v", err)
	}
	return tok
}

// adminSuiteUserRole reads back the current role for userID so tests can
// assert that a role mutation actually persisted (or didn't).
func adminSuiteUserRole(t *testing.T, d *db.DB, userID uuid.UUID) string {
	t.Helper()
	var role string
	if err := d.Pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&role); err != nil {
		t.Fatalf("read back admin-suite user role: %v", err)
	}
	return role
}

// adminSuiteDo performs an HTTP request against app with an optional bearer
// token and optional JSON body, returning the status code and the decoded
// JSON body (as a generic map). It disables Fiber's default 1s test timeout
// since this suite's tests hit a real, potentially contended, Postgres
// instance shared with other concurrently-running test suites.
func adminSuiteDo(t *testing.T, app *fiber.App, method, path, token string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	decoded := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err != io.EOF {
		t.Fatalf("decode response body: %v", err)
	}
	return resp.StatusCode, decoded
}

// newAdminRoutesTestApp mounts /admin/bootstrap, /admin/users and
// /admin/users/:id/role exactly as internal/api/api.go wires AdminHandler:
// RequireAuth only for bootstrap (self-service), RequireAuth+RequireRole
// admin for the rest.
func newAdminRoutesTestApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewAdminHandler(cfg, d)
	adminGroup := app.Group("/admin", auth.RequireAuth(cfg.JWTSecret))
	adminGroup.Post("/bootstrap", h.BootstrapAdmin())
	adminGroup.Get("/users", auth.RequireRole("admin"), h.ListUsers())
	adminGroup.Put("/users/:id/role", auth.RequireRole("admin"), h.SetUserRole())
	return app
}

// ---------------------------------------------------------------------------
// ListUsers / SetUserRole RBAC matrix
// ---------------------------------------------------------------------------

func TestAdminListUsers_RBAC(t *testing.T) {
	d := testDB(t)
	app := newAdminRoutesTestApp(adminSuiteConfig("unused-bootstrap-token"), d)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "GET", "/admin/users", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")
		status, _ := adminSuiteDo(t, app, "GET", "/admin/users", tok, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("authenticated admin succeeds", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")
		status, body := adminSuiteDo(t, app, "GET", "/admin/users", tok, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		if _, ok := body["users"]; !ok {
			t.Errorf("expected \"users\" key in response, got %v", body)
		}
	})
}

func TestAdminSetUserRole_RBAC(t *testing.T) {
	d := testDB(t)
	app := newAdminRoutesTestApp(adminSuiteConfig("unused-bootstrap-token"), d)
	target := adminSuiteInsertUser(t, d, "contributor")

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "PUT", "/admin/users/"+target.String()+"/role", "", map[string]string{"role": "maintainer"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
		if role := adminSuiteUserRole(t, d, target); role != "contributor" {
			t.Errorf("role changed despite missing auth: role = %q", role)
		}
	})

	t.Run("authenticated non-admin is forbidden", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "maintainer")
		tok := adminSuiteToken(t, uid, "maintainer")
		status, _ := adminSuiteDo(t, app, "PUT", "/admin/users/"+target.String()+"/role", tok, map[string]string{"role": "admin"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
		if role := adminSuiteUserRole(t, d, target); role != "contributor" {
			t.Errorf("role changed despite non-admin caller: role = %q", role)
		}
	})

	t.Run("admin sets a valid role and it persists", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		status, body := adminSuiteDo(t, app, "PUT", "/admin/users/"+target.String()+"/role", adminTok, map[string]string{"role": "maintainer"})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		if body["ok"] != true {
			t.Errorf("ok = %v, want true", body["ok"])
		}
		if role := adminSuiteUserRole(t, d, target); role != "maintainer" {
			t.Errorf("role in DB = %q, want maintainer (change did not persist)", role)
		}
	})

	t.Run("admin rejects an unknown role value", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		status, body := adminSuiteDo(t, app, "PUT", "/admin/users/"+target.String()+"/role", adminTok, map[string]string{"role": "superadmin"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_role" {
			t.Errorf("error = %v, want invalid_role", body["error"])
		}
	})

	t.Run("admin gets 404 for a nonexistent user id", func(t *testing.T) {
		adminUID := adminSuiteInsertUser(t, d, "admin")
		adminTok := adminSuiteToken(t, adminUID, "admin")
		status, body := adminSuiteDo(t, app, "PUT", "/admin/users/"+uuid.NewString()+"/role", adminTok, map[string]string{"role": "maintainer"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "user_not_found" {
			t.Errorf("error = %v, want user_not_found", body["error"])
		}
	})
}

// ---------------------------------------------------------------------------
// BootstrapAdmin
// ---------------------------------------------------------------------------

func TestAdminBootstrapAdmin(t *testing.T) {
	d := testDB(t)
	const goodToken = "admin-suite-bootstrap-token-xyz"
	app := newAdminRoutesTestApp(adminSuiteConfig(goodToken), d)

	t.Run("unauthenticated caller is rejected before token check", func(t *testing.T) {
		status, _ := adminSuiteDo(t, app, "POST", "/admin/bootstrap", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("correct token promotes a contributor to admin", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")

		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Admin-Bootstrap-Token", goodToken)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["ok"] != true {
			t.Errorf("ok = %v, want true", body["ok"])
		}
		if body["role"] != "admin" {
			t.Errorf("role = %v, want admin", body["role"])
		}
		tokenStr, _ := body["token"].(string)
		if tokenStr == "" {
			t.Errorf("expected a non-empty fresh JWT in response")
		}
		if role := adminSuiteUserRole(t, d, uid); role != "admin" {
			t.Errorf("role in DB = %q, want admin (promotion did not persist)", role)
		}
	})

	t.Run("already-admin caller gets a fresh token without error", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "admin")
		tok := adminSuiteToken(t, uid, "admin")

		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Admin-Bootstrap-Token", goodToken)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["role"] != "admin" {
			t.Errorf("role = %v, want admin", body["role"])
		}
	})

	t.Run("wrong token is rejected and does not promote", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")

		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Admin-Bootstrap-Token", "totally-wrong-token")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body["error"] != "invalid_bootstrap_token" {
			t.Errorf("error = %v, want invalid_bootstrap_token", body["error"])
		}
		if role := adminSuiteUserRole(t, d, uid); role != "contributor" {
			t.Errorf("role in DB = %q, want contributor (should not be promoted on wrong token)", role)
		}
	})

	t.Run("missing token header is rejected same as wrong token", func(t *testing.T) {
		uid := adminSuiteInsertUser(t, d, "contributor")
		tok := adminSuiteToken(t, uid, "contributor")

		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		// Deliberately no X-Admin-Bootstrap-Token header.
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
		if role := adminSuiteUserRole(t, d, uid); role != "contributor" {
			t.Errorf("role in DB = %q, want contributor (should not be promoted on missing token)", role)
		}
	})
}

// TestAdminBootstrapAdmin_EmptyConfiguredToken captures the ACTUAL behavior
// of BootstrapAdmin when cfg.AdminBootstrapToken is configured as an empty
// string. Read internal/handlers/admin.go:112-114: the handler checks
// `h.cfg.AdminBootstrapToken == ""` and returns 503 bootstrap_not_configured
// BEFORE it ever compares the submitted header token (admin.go:118-122).
// That means an empty submitted token can NEVER "match" an empty configured
// token over HTTP - the early-return guard closes that gap. This test
// documents/locks in that safe behavior; if this test ever starts failing
// because the guard was removed or reordered, that would reintroduce a
// free-admin-promotion vulnerability.
func TestAdminBootstrapAdmin_EmptyConfiguredToken(t *testing.T) {
	d := testDB(t)
	app := newAdminRoutesTestApp(adminSuiteConfig(""), d)

	uid := adminSuiteInsertUser(t, d, "contributor")
	tok := adminSuiteToken(t, uid, "contributor")

	t.Run("empty submitted token against empty configured token is refused, not granted", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		// Deliberately omitted: X-Admin-Bootstrap-Token header ends up "".
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d (bootstrap_not_configured guard)", resp.StatusCode, fiber.StatusServiceUnavailable)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body["error"] != "bootstrap_not_configured" {
			t.Errorf("error = %v, want bootstrap_not_configured", body["error"])
		}
		if role := adminSuiteUserRole(t, d, uid); role != "contributor" {
			t.Errorf("role in DB = %q, want contributor (must not be silently promoted for free)", role)
		}
	})

	t.Run("explicit empty-string token header against empty configured token is also refused", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Admin-Bootstrap-Token", "")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d (bootstrap_not_configured guard)", resp.StatusCode, fiber.StatusServiceUnavailable)
		}
		if role := adminSuiteUserRole(t, d, uid); role != "contributor" {
			t.Errorf("role in DB = %q, want contributor (must not be silently promoted for free)", role)
		}
	})
}
