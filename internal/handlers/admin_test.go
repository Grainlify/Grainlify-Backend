package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// openTestPool connects to TEST_DB_URL and applies migrations.
// It skips the test if TEST_DB_URL is not set.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set – skipping integration tests")
		return nil
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)

	err = pool.Ping(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}

	// Apply all migrations so the schema is up to date.
	err = migrate.Up(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatalf("migrate.Up: %v", err)
	}

	t.Cleanup(pool.Close)
	return pool
}

func setupTestApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})
	admin := handlers.NewAdminHandler(cfg, d)
	// Requires standard auth middleware
	app.Post("/admin/bootstrap", auth.RequireAuth(cfg.JWTSecret), admin.BootstrapAdmin())
	return app
}

// newAdminListApp wires ListUsers behind a fiber app using a DB with no pool.
// Pagination parameters are validated before the DB availability check, so the
// limit/offset bounds are exercised without requiring a live database:
//   - valid params pass validation and reach the db check (503 db_not_configured)
//   - out-of-range params are rejected up front (400)
func newAdminListApp() *fiber.App {
	h := handlers.NewAdminHandler(config.Config{}, &db.DB{})
	app := fiber.New()
	app.Get("/admin/users", h.ListUsers())
	return app
}

func decodeError(t *testing.T, app *fiber.App, target string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", target, nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error
}

func TestAdminListUsers_DefaultParamsAccepted(t *testing.T) {
	app := newAdminListApp()
	// No params: defaults are applied and validation passes, so the handler
	// proceeds to the db check (which reports not configured rather than 400).
	status, errMsg := decodeError(t, app, "/admin/users")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestAdminListUsers_BoundedParamsAccepted(t *testing.T) {
	app := newAdminListApp()
	// Explicit, in-range values pass validation.
	status, errMsg := decodeError(t, app, "/admin/users?limit=25&offset=25")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestAdminListUsers_LimitAboveMaxAccepted(t *testing.T) {
	app := newAdminListApp()
	// A limit above the cap is clamped, not rejected.
	status, errMsg := decodeError(t, app, "/admin/users?limit=10000")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestAdminListUsers_ZeroLimitAccepted(t *testing.T) {
	app := newAdminListApp()
	// limit=0 falls back to the default rather than erroring.
	status, errMsg := decodeError(t, app, "/admin/users?limit=0")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestAdminListUsers_NegativeOffsetRejected(t *testing.T) {
	app := newAdminListApp()
	// A negative offset is rejected with 400 before any DB work.
	status, errMsg := decodeError(t, app, "/admin/users?offset=-1")
	assert.Equal(t, fiber.StatusBadRequest, status)
	assert.Equal(t, "offset must be non-negative", errMsg)
}

func TestBootstrapAdmin_EnvNotDev(t *testing.T) {
	cfg := config.Config{
		Env:                 "production",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, &db.DB{})
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "bootstrap_disabled_in_env", body["error"])
}

func TestBootstrapAdmin_EmptyConfiguredToken(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "",
	}

	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, &db.DB{})
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", "some-token")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "bootstrap_not_configured", body["error"])
}

func TestBootstrapAdmin_ShortConfiguredToken(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "too-short-12345", // Less than 32 chars
	}

	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, &db.DB{})
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "bootstrap_token_too_weak", body["error"])
}

func TestBootstrapAdmin_TokenMismatch(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, &db.DB{})
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", "wrong-token-value-here-which-is-longer-or-different")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "invalid_bootstrap_token", body["error"])
}

func TestBootstrapAdmin_NoDB(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	// DB is nil
	app := setupTestApp(cfg, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "db_not_configured", body["error"])
}

func TestBootstrapAdmin_Integration(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	ctx := context.Background()

	// 1. Seed a user in the database
	var userID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO users (role) VALUES ('contributor') RETURNING id
	`).Scan(&userID)
	require.NoError(t, err)

	// 2. Issue a valid JWT token for this user
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, d)

	// 3. Perform a bootstrap attempt
	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, true, body["ok"])
	assert.Equal(t, "admin", body["role"])
	assert.NotEmpty(t, body["token"])

	// 4. Verify in the database that the user is now an admin
	var dbRole string
	err = pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&dbRole)
	require.NoError(t, err)
	assert.Equal(t, "admin", dbRole)

	// 5. Run again as an existing admin to verify the no-op fast-path
	req2 := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req2.Header.Set("Authorization", "Bearer "+token) // using previous token still works or new token
	req2.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp2, err := app.Test(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, fiber.StatusOK, resp2.StatusCode)

	var body2 map[string]interface{}
	err = json.NewDecoder(resp2.Body).Decode(&body2)
	require.NoError(t, err)
	assert.Equal(t, true, body2["ok"])
	assert.Equal(t, "admin", body2["role"])
}

func TestBootstrapAdmin_JWTNotConfigured(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "", // Empty JWT Secret
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})
	admin := handlers.NewAdminHandler(cfg, &db.DB{})
	// Mock a direct route to test BootstrapAdmin when RequireAuth isn't blocking it
	app.Post("/admin/bootstrap", func(c *fiber.Ctx) error {
		// Mock userID inject in context since RequireAuth is omitted
		c.Locals(auth.LocalUserID, uuid.New().String())
		return admin.BootstrapAdmin()(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "jwt_not_configured", body["error"])
}

func TestBootstrapAdmin_UserNotFound(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	// Issue token for a random userID not in the database
	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, d)

	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "user_not_found", body["error"])
}

func TestBootstrapAdmin_InvalidUserID(t *testing.T) {
	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})
	admin := handlers.NewAdminHandler(cfg, &db.DB{})
	app.Post("/admin/bootstrap", func(c *fiber.Ctx) error {
		// Mock an invalid UUID in context
		c.Locals(auth.LocalUserID, "not-a-valid-uuid")
		return admin.BootstrapAdmin()(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "invalid_user", body["error"])
}

func TestBootstrapAdmin_DatabaseQueryError(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	cfg := config.Config{
		Env:                 "dev",
		JWTSecret:           "my-jwt-test-secret",
		AdminBootstrapToken: "a-very-long-token-32-characters-long-12345",
	}

	// Issue token for a user ID
	userID := uuid.New()
	token, err := auth.IssueJWT(cfg.JWTSecret, userID, "contributor", "evm", "0x123", time.Hour)
	require.NoError(t, err)

	app := setupTestApp(cfg, d)

	// Close database pool to trigger database connection/query error
	pool.Close()

	req := httptest.NewRequest(http.MethodPost, "/admin/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Admin-Bootstrap-Token", cfg.AdminBootstrapToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "bootstrap_failed", body["error"])
}

func TestListUsers_NoDB(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, nil)
	app.Get("/admin/users", admin.ListUsers())

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

func TestListUsers_Integration(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Get("/admin/users", admin.ListUsers())

	// Seed a user
	ctx := context.Background()
	var userID uuid.UUID
	err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&userID)
	require.NoError(t, err)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	var result map[string][]map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	assert.NotEmpty(t, result["users"])

	found := false
	for _, u := range result["users"] {
		if u["id"] == userID.String() {
			found = true
			assert.Equal(t, "contributor", u["role"])
			break
		}
	}
	assert.True(t, found, "should find seeded user in list")
}

func TestSetUserRole_NoDB(t *testing.T) {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, nil)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	resp, err := app.Test(httptest.NewRequest(http.MethodPut, "/admin/users/"+uuid.New().String()+"/role", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
}

func TestSetUserRole_InvalidUserID(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	resp, err := app.Test(httptest.NewRequest(http.MethodPut, "/admin/users/invalid-uuid/role", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestSetUserRole_InvalidJSON(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	req := httptest.NewRequest(http.MethodPut, "/admin/users/"+uuid.New().String()+"/role", strings.NewReader("malformed-json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestSetUserRole_InvalidRole(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	reqBody := `{"role": "invalid-role-name"}`
	req := httptest.NewRequest(http.MethodPut, "/admin/users/"+uuid.New().String()+"/role", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestSetUserRole_UserNotFound(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	reqBody := `{"role": "maintainer"}`
	req := httptest.NewRequest(http.MethodPut, "/admin/users/"+uuid.New().String()+"/role", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestSetUserRole_Integration(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	// Seed a user
	ctx := context.Background()
	var userID uuid.UUID
	err := pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&userID)
	require.NoError(t, err)

	reqBody := `{"role": "maintainer"}`
	req := httptest.NewRequest(http.MethodPut, "/admin/users/"+userID.String()+"/role", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	var dbRole string
	err = pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&dbRole)
	require.NoError(t, err)
	assert.Equal(t, "maintainer", dbRole)
}

func TestListUsers_DatabaseError(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Get("/admin/users", admin.ListUsers())

	pool.Close()

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}

func TestSetUserRole_DatabaseError(t *testing.T) {
	pool := openTestPool(t)
	d := &db.DB{Pool: pool}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	admin := handlers.NewAdminHandler(config.Config{}, d)
	app.Put("/admin/users/:id/role", admin.SetUserRole())

	pool.Close()

	reqBody := `{"role": "maintainer"}`
	req := httptest.NewRequest(http.MethodPut, "/admin/users/"+uuid.New().String()+"/role", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}
