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

// newAdminRoutesTestApp mounts /admin/users and /admin/users/:id/role exactly
// as internal/api/api.go wires AdminHandler: RequireAuth plus an admin role
// check on every route, with no self-service exception. There used to be a
// /admin/bootstrap here, mounted with RequireAuth alone; it was removed.
func newAdminRoutesTestApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewAdminHandler(cfg, d)
	adminGroup := app.Group("/admin", auth.RequireAuth(cfg.JWTSecret))
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

// The two bootstrap suites that lived here were removed with the endpoint.
//
// They covered a real thing carefully - first grant, wrong token, missing
// header, and an empty configured token not being a free promotion - but all
// four described an endpoint that no longer exists. Keeping them would have
// meant reinstating /admin/bootstrap to make the suite compile, which is the
// tail wagging the dog.
//
// What replaced them: TestBootstrapRouteIsGone in internal/api asserts the
// route cannot come back, and break_glass_test.go verifies the recovery path
// that replaced it against a real database.
