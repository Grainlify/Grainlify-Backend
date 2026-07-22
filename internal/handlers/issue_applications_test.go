package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// idempotencyTestPool is a mock DBPool that returns a configured scan error from QueryRow.
// Used to simulate idempotency cache-miss scenarios (pgx.ErrNoRows) and unexpected DB errors.
type idempotencyTestPool struct {
	scanErr error
}

func (p *idempotencyTestPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (p *idempotencyTestPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (p *idempotencyTestPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &idempotencyTestRow{err: p.scanErr}
}
func (p *idempotencyTestPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}
func (p *idempotencyTestPool) Ping(ctx context.Context) error  { return nil }
func (p *idempotencyTestPool) Close()                          {}
func (p *idempotencyTestPool) Config() *pgxpool.Config          { return nil }

// idempotencyTestRow is a mock pgx.Row that returns the configured error on Scan.
type idempotencyTestRow struct {
	err error
}

func (r *idempotencyTestRow) Scan(dest ...any) error {
	return r.err
}

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

// TestIdempotencyCacheMiss_NoRows verifies that a pgx.ErrNoRows from the idempotency
// key lookup is treated as a normal cache miss, not an unexpected DB error.
// The handler proceeds past the idempotency check into the normal application flow
// (which fails at the GitHub linked-account lookup since all queries return pgx.ErrNoRows).
// This locks in the behavior against the fragile string-matching check it replaced.
func TestIdempotencyCacheMiss_NoRows(t *testing.T) {
	pool := &idempotencyTestPool{scanErr: pgx.ErrNoRows}
	app := newIssueApplicationsAppWithPool(pool)

	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-123")

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should NOT be 503 (db_not_configured) — the pool is non-nil, so the idempotency
	// cache-miss path was exercised. The handler falls through to the normal flow,
	// where the GitHub linked-account lookup also returns pgx.ErrNoRows → 400.
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "github_not_linked", body["error"])
}

// TestIdempotencyCacheMiss_OtherError verifies that a non-pgx.ErrNoRows error during
// the idempotency key lookup logs a warning but still allows the request to fall through
// to normal processing (the handler does NOT fail the request for DB lookup errors).
func TestIdempotencyCacheMiss_OtherError(t *testing.T) {
	pool := &idempotencyTestPool{scanErr: fmt.Errorf("connection refused")}
	app := newIssueApplicationsAppWithPool(pool)

	req := httptest.NewRequest(http.MethodPost, "/projects/11111111-1111-1111-1111-111111111111/issues/1/apply", strings.NewReader(`{"message":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-123")

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)
	defer resp.Body.Close()

	// The handler should NOT fail the request at the idempotency check. It falls through
	// to normal processing, which eventually fails at the GitHub linked-account lookup.
	// A warning log is emitted (not asserted here — requires log capture infrastructure).
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Equal(t, "github_not_linked", body["error"])
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

// newIssueApplicationsAppWithPool is like newIssueApplicationsApp but uses the provided pool
// instead of a nil pool, allowing tests to exercise the idempotency cache-miss path.
func newIssueApplicationsAppWithPool(pool db.DBPool) *fiber.App {
	cfg := config.Config{
		TokenEncKeyB64: "dGVzdGtleXRlc3RrZXl0ZXN0a2V5dGVzdGtleQ==", // "testkeykey..." base64 (valid length)
	}
	h := handlers.NewIssueApplicationsHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New()
	app.Post("/projects/:id/issues/:number/apply", h.Apply())
	return app
}
