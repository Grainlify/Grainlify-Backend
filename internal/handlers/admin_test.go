package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

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

func decodePostError(t *testing.T, app *fiber.App, target string, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req)
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

func newAdminBootstrapApp(t *testing.T, cfg config.Config, pool *pgxpool.Pool, injectUserID string) *fiber.App {
	h := handlers.NewAdminHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New()
	app.Post("/admin/bootstrap", func(c *fiber.Ctx) error {
		if injectUserID != "" {
			c.Locals(auth.LocalUserID, injectUserID)
		}
		return c.Next()
	}, h.BootstrapAdmin())
	return app
}

func TestAdminBootstrap_DisabledWhenTokenEmpty(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{
		AdminBootstrapToken: "",
		JWTSecret:           "super-secret-jwt-key-for-test-32-chars-long",
	}
	app := newAdminBootstrapApp(t, cfg, pool, "11111111-2222-3333-4444-555555555555")

	status, errMsg := decodePostError(t, app, "/admin/bootstrap", map[string]string{
		"X-Admin-Bootstrap-Token": "some-token",
	})
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "bootstrap_not_configured", errMsg)
}

func TestAdminBootstrap_DisabledWhenTokenShort(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{
		// Shorter than 32 characters
		AdminBootstrapToken: "short-token",
		JWTSecret:           "super-secret-jwt-key-for-test-32-chars-long",
	}
	app := newAdminBootstrapApp(t, cfg, pool, "11111111-2222-3333-4444-555555555555")

	status, errMsg := decodePostError(t, app, "/admin/bootstrap", map[string]string{
		"X-Admin-Bootstrap-Token": "short-token",
	})
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "bootstrap_not_configured", errMsg)
}

func TestAdminBootstrap_InvalidTokenReturnsUnauthorized(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{
		// Exactly 32 characters (strong token)
		AdminBootstrapToken: "strong-token-1234567890123456789",
		JWTSecret:           "super-secret-jwt-key-for-test-32-chars-long",
	}
	app := newAdminBootstrapApp(t, cfg, pool, "11111111-2222-3333-4444-555555555555")

	status, errMsg := decodePostError(t, app, "/admin/bootstrap", map[string]string{
		"X-Admin-Bootstrap-Token": "incorrect-token-1234567890123456",
	})
	assert.Equal(t, fiber.StatusUnauthorized, status)
	assert.Equal(t, "invalid_bootstrap_token", errMsg)
}

func TestAdminBootstrap_InvalidUserContext(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{
		AdminBootstrapToken: "strong-token-1234567890123456789",
		JWTSecret:           "super-secret-jwt-key-for-test-32-chars-long",
	}
	// No injected user ID or invalid UUID format
	app := newAdminBootstrapApp(t, cfg, pool, "invalid-uuid")

	status, errMsg := decodePostError(t, app, "/admin/bootstrap", map[string]string{
		"X-Admin-Bootstrap-Token": "strong-token-1234567890123456789",
	})
	assert.Equal(t, fiber.StatusUnauthorized, status)
	assert.Equal(t, "invalid_user", errMsg)
}
