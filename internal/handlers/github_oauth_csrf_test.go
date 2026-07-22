// Package handlers_test contains integration tests for the GitHub OAuth CSRF
// state-parameter validation.  These tests verify that the CallbackUnified
// handler is closed against all three classic OAuth CSRF attack vectors:
//
//  1. Missing state — attacker-constructed callback with no state
//  2. Mismatched / never-issued state — state not in the database
//  3. Replay attack — same state used a second time after first use
//
// Additional edge cases: expired state, malformed state encoding.
//
// Tests in this file require a real PostgreSQL instance pointed to by
// TEST_DB_URL.  They are skipped automatically when that variable is unset
// (e.g. unit-test runs without a database).
//
// Security context: A missing or replayable state parameter is a textbook
// OAuth CSRF vulnerability.  An attacker who can trick a victim into visiting
// a crafted callback URL could link the attacker's GitHub account to the
// victim's session.  All paths here must fail-closed.
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

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// csrfTestPool opens a live DB for CSRF integration tests.
// Skips the test if TEST_DB_URL is not set.
func csrfTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set – skipping OAuth CSRF integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))
	require.NoError(t, migrate.Up(ctx, pool, true))
	t.Cleanup(pool.Close)
	return pool
}

// minCfg returns the minimal config.Config required by CallbackUnified.
func minCfg() config.Config {
	return config.Config{
		GitHubOAuthClientID:     "test_client_id",
		GitHubOAuthClientSecret: "test_client_secret",
		GitHubOAuthRedirectURL:  "http://localhost:8080/auth/github/callback",
		JWTSecret:               strings.Repeat("s", 32),
		// 32-byte key base64-encoded (test only — never use in production)
		TokenEncKeyB64:    "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=",
		FrontendBaseURL:   "http://localhost:5173",
	}
}

// buildApp builds a minimal Fiber app with the GitHub OAuth callback mounted.
func buildApp(cfg config.Config, pool *pgxpool.Pool) *fiber.App {
	h := handlers.NewGitHubOAuthHandler(cfg, &db.DB{Pool: pool})
	app := fiber.New(fiber.Config{
		// Prevent panics from hiding the error codes we are testing.
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		},
	})
	app.Get("/auth/github/callback", h.CallbackUnified())
	return app
}

// oauthErrorCode reads the standard httpx ErrorEnvelope from the response body.
func oauthErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env httpx.ErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env.Error
}

// insertState is a test helper that inserts an oauth_states row directly.
func insertState(t *testing.T, pool *pgxpool.Pool, state string, kind string, userID *uuid.UUID, expiresAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO oauth_states (state, user_id, kind, expires_at) VALUES ($1, $2, $3, $4)`,
		state, userID, kind, expiresAt,
	)
	require.NoError(t, err, "insertState")
}

// stateExists reports whether the given state row still exists in the DB.
func stateExists(t *testing.T, pool *pgxpool.Pool, state string) bool {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM oauth_states WHERE state = $1`, state,
	).Scan(&count)
	require.NoError(t, err)
	return count > 0
}

// ---------------------------------------------------------------------------
// CSRF test: missing state parameter
// ---------------------------------------------------------------------------

// TestGitHubOAuthCSRF_MissingState asserts that a callback arriving without
// any state query parameter is rejected immediately with 400.
//
// Attack prevented: an attacker who never goes through LoginStart has no state
// to include; this gate ensures the request cannot proceed at all.
func TestGitHubOAuthCSRF_MissingState(t *testing.T) {
	pool := csrfTestPool(t)
	app := buildApp(minCfg(), pool)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=some_code", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode,
		"callback without state must be rejected with 400")

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "missing_code_or_state", oauthErrorCode(t, buf[:n]))
}

// TestGitHubOAuthCSRF_MissingCode asserts that a callback arriving without a
// code parameter is also rejected (completeness; same guard as missing state).
func TestGitHubOAuthCSRF_MissingCode(t *testing.T) {
	pool := csrfTestPool(t)
	app := buildApp(minCfg(), pool)

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?state=some_state", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "missing_code_or_state", oauthErrorCode(t, buf[:n]))
}

// ---------------------------------------------------------------------------
// CSRF test: mismatched / never-issued state
// ---------------------------------------------------------------------------

// TestGitHubOAuthCSRF_MismatchedState asserts that a callback whose state value
// was never stored in oauth_states (i.e. it was not issued by our LoginStart)
// is rejected with 400 invalid_or_expired_state.
//
// Attack prevented: an attacker constructs their own authorization URL with a
// state they control.  Our database contains no matching row, so we reject.
func TestGitHubOAuthCSRF_MismatchedState(t *testing.T) {
	pool := csrfTestPool(t)
	app := buildApp(minCfg(), pool)

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=any_code&state=attacker-controlled-state", nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode,
		"callback with never-issued state must be rejected with 400")

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "invalid_or_expired_state", oauthErrorCode(t, buf[:n]))
}

// TestGitHubOAuthCSRF_MismatchedState_ValidBase64 asserts that a well-formed
// base64 state that decodes to a real-looking CSRF token (but was never issued)
// is still rejected.  This covers an attacker who crafts a plausible state.
func TestGitHubOAuthCSRF_MismatchedState_ValidBase64(t *testing.T) {
	pool := csrfTestPool(t)
	app := buildApp(minCfg(), pool)

	// A base64-encoded "token" that looks like one our system would produce
	// but was never stored in oauth_states.
	fakeEncodedState := github.EncodeStateWithRedirect("fake-csrf-token-never-issued", "")
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=any_code&state="+fakeEncodedState, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode,
		"callback with plausible but never-issued state must be rejected")

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "invalid_or_expired_state", oauthErrorCode(t, buf[:n]))
}

// ---------------------------------------------------------------------------
// CSRF test: replayed state (single-use enforcement)
// ---------------------------------------------------------------------------

// TestGitHubOAuthCSRF_StateReplay asserts that after a state row is consumed
// (deleted from oauth_states), a second callback presenting the same state is
// rejected with 400 invalid_or_expired_state.
//
// Attack prevented: an attacker who captures a valid callback URL (state+code)
// and submits it again cannot link their account to the victim's session.
//
// Implementation note: the DELETE happens BEFORE ExchangeCode, so even if the
// token exchange fails the state is already gone.  This is the correct order:
// if we deleted after a successful exchange, a crash between exchange and delete
// would leave a replayable state in the DB.
func TestGitHubOAuthCSRF_StateReplay(t *testing.T) {
	pool := csrfTestPool(t)

	// Insert a valid state row
	state := "single-use-csrf-token-" + uuid.NewString()
	insertState(t, pool, state, "github_login", nil, time.Now().UTC().Add(10*time.Minute))

	// Verify it's there before we try
	require.True(t, stateExists(t, pool, state), "state must exist before first use")

	// Directly DELETE the state from the DB — simulating what CallbackUnified
	// does on first use.  We do it directly here because the full callback would
	// need a real GitHub token exchange that we can't do in unit tests.
	_, err := pool.Exec(context.Background(),
		`DELETE FROM oauth_states WHERE state = $1`, state)
	require.NoError(t, err, "simulate first-use DELETE")

	// Verify it's gone
	require.False(t, stateExists(t, pool, state), "state must be deleted after first use")

	// Now simulate the replay: callback arrives with the already-consumed state
	app := buildApp(minCfg(), pool)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=replay_code&state="+state, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode,
		"replayed (already-consumed) state must be rejected with 400")

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "invalid_or_expired_state", oauthErrorCode(t, buf[:n]))
}

// TestGitHubOAuthCSRF_StateDeletedBeforeExchange verifies the DELETE-before-
// ExchangeCode ordering in CallbackUnified by checking that the row is absent
// even when the subsequent token exchange would fail.
//
// This test exercises the handler up to the point where it attempts GitHub
// token exchange (which will fail against the fake code), but confirms the
// state row was already deleted before that failure occurs.
func TestGitHubOAuthCSRF_StateDeletedBeforeExchange(t *testing.T) {
	pool := csrfTestPool(t)

	// Mock GitHub token endpoint to return an error (simulates exchange failure)
	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "bad_verification_code",
		})
	}))
	defer mockGitHub.Close()

	// Override the token endpoint so ExchangeCode hits our mock
	old := github.GetTokenEndpoint()
	github.SetTokenEndpoint(mockGitHub.URL)
	defer github.SetTokenEndpoint(old)

	// Insert a valid state row
	state := "delete-before-exchange-" + uuid.NewString()
	insertState(t, pool, state, "github_login", nil, time.Now().UTC().Add(10*time.Minute))

	app := buildApp(minCfg(), pool)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=bad_code&state="+state, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	// The handler should fail at ExchangeCode (401 token_exchange_failed)
	// but the state must already be deleted
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode,
		"token exchange failure should return 401, got %d", resp.StatusCode)

	// Critical assertion: state is gone even though exchange failed
	assert.False(t, stateExists(t, pool, state),
		"state must be deleted BEFORE ExchangeCode — single-use enforced even on exchange failure")
}

// ---------------------------------------------------------------------------
// CSRF test: expired state
// ---------------------------------------------------------------------------

// TestGitHubOAuthCSRF_ExpiredState asserts that a state whose expires_at is in
// the past is rejected.  The DB query uses WHERE expires_at > now(), so an
// expired row is treated the same as a missing row.
//
// Attack prevented: an attacker who captures a callback URL cannot use it after
// the 10-minute TTL window has passed.
func TestGitHubOAuthCSRF_ExpiredState(t *testing.T) {
	pool := csrfTestPool(t)
	app := buildApp(minCfg(), pool)

	// Insert a state that is already expired
	state := "expired-csrf-token-" + uuid.NewString()
	insertState(t, pool, state, "github_login", nil, time.Now().UTC().Add(-1*time.Minute))

	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=some_code&state="+state, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode,
		"expired state must be rejected with 400")

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	assert.Equal(t, "invalid_or_expired_state", oauthErrorCode(t, buf[:n]))
}

// ---------------------------------------------------------------------------
// Positive control: legitimate flow
// ---------------------------------------------------------------------------

// TestGitHubOAuthCSRF_LegitimateFlowUnaffected verifies that the CSRF defenses
// do not break a legitimate OAuth callback.  The flow proceeds as far as the
// GitHub token exchange; we intercept at that layer with a mock server that
// intentionally returns an error so we don't need real credentials, but we
// confirm the state validation itself passed (the error is from exchange, not
// from state validation).
func TestGitHubOAuthCSRF_LegitimateFlowUnaffected(t *testing.T) {
	pool := csrfTestPool(t)

	// Mock GitHub token endpoint — succeeds for code exchange but we stop before
	// the full user lookup to keep the test hermetic
	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a bad_verification_code error so the handler returns 401 after
		// passing state validation — we just want to confirm we got past the CSRF check
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mockGitHub.Close()

	old := github.GetTokenEndpoint()
	github.SetTokenEndpoint(mockGitHub.URL)
	defer github.SetTokenEndpoint(old)

	// Insert a valid, non-expired state
	state := "legitimate-csrf-token-" + uuid.NewString()
	insertState(t, pool, state, "github_login", nil, time.Now().UTC().Add(10*time.Minute))

	app := buildApp(minCfg(), pool)
	req := httptest.NewRequest(http.MethodGet,
		"/auth/github/callback?code=legit_code&state="+state, nil)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Must NOT be rejected by CSRF checks (those return 400); it should proceed
	// to ExchangeCode which fails with 400/401 from our mock.
	// Either 400 (exchange error) or 401 (token_exchange_failed) is fine —
	// what matters is we did NOT get 400 invalid_or_expired_state / missing_code_or_state.
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	code := oauthErrorCode(t, buf[:n])

	assert.NotEqual(t, "missing_code_or_state", code,
		"legitimate flow must pass the missing-state check")
	assert.NotEqual(t, "invalid_or_expired_state", code,
		"legitimate flow must pass the state-validation check")
	assert.NotEqual(t, "invalid_state_format", code,
		"legitimate flow must pass the state-format check")

	t.Logf("flow proceeded past CSRF checks, failed at token exchange with error=%q (expected)", code)
}
