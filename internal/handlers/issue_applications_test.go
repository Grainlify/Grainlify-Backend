package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// TestIdempotencyKeyTooLong verifies that an idempotency key exceeding 255 characters is rejected.
func TestIdempotencyKeyTooLong(t *testing.T) {
	app := newIssueApplicationsApp()
	longKey := strings.Repeat("a", 256)
	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", longKey)

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "idempotency_key_too_long", body["error"])
}

// TestIdempotencyKeyValidLengthAccepted verifies that a valid-length idempotency key (≤255 chars) is accepted.
// Since the DB is not configured, the request will reach the DB check and return 503, not 400.
func TestIdempotencyKeyValidLengthAccepted(t *testing.T) {
	app := newIssueApplicationsApp()
	validKey := strings.Repeat("a", 255)
	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", validKey)

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should NOT be rejected for key length — will fail at DB check (503) instead.
	assert.NotEqual(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "db_not_configured", body["error"])
}

// TestIdempotencyKeyAbsentAllowsNormalFlow verifies that when no Idempotency-Key header is provided,
// the handler proceeds to the normal flow (DB check in this case, since DB is nil).
func TestIdempotencyKeyAbsentAllowsNormalFlow(t *testing.T) {
	app := newIssueApplicationsApp()
	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	// No Idempotency-Key header set.

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should proceed to DB check and return 503 (not 400 for missing key).
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "db_not_configured", body["error"])
}

// TestIdempotencyKeyEmptyRejected verifies that an empty idempotency key (header present but value empty after trim)
// is NOT rejected at the validation stage — an empty string after trim has length 0, which is ≤255, so it passes
// length validation. This test confirms that an empty key is treated as "no key provided" and falls through.
func TestIdempotencyKeyEmptyTreatedAsAbsent(t *testing.T) {
	app := newIssueApplicationsApp()
	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "   ") // Whitespace only, trims to empty string.

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Empty key (after trim) is treated as no key provided — should proceed to DB check.
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "db_not_configured", body["error"])
}

// newIssueApplicationsApp wires the Apply handler behind a fiber app using a DB with no pool.
// The idempotency key validation happens before the DB availability check, so invalid keys
// are rejected up front (400) while valid keys fall through to the DB check (503).
func newIssueApplicationsApp() *fiber.App {
	cfg := config.Config{
		TokenEncKeyB64: "dGVzdGtleXRlc3RrZXl0ZXN0a2V5dGVzdGtleQ==", // "testkeykey..." base64 (valid length)
	}
	h := handlers.NewIssueApplicationsHandler(cfg, &db.DB{})
	app := fiber.New()
	app.Post("/projects/:id/issues/:number/apply", h.Apply())
	return app
}
