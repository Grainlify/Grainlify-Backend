package handlers_test

// Scope note: covers handlers.KYCHandler (internal/handlers/kyc.go) - Start()
// and Status().
//
// Every identifier defined in this file is prefixed with "kycSuite" so it
// cannot collide with fixtures other concurrently-developed *_test.go files
// in this package define for their own domains.
//
// Start()'s actual session-creation happy path (and the "existing session
// blocks a new one" 409 conflict path, which also requires a real Didit API
// round-trip to decide whether the old session still exists) are NOT
// reachable here: internal/didit/client.go:13 hardcodes
// `const BaseURL = "https://verification.didit.me/v2"` with no injectable
// seam (unlike e.g. internal/github/oauth.go's tokenEndpoint var, which
// exists specifically so that package's own tests can override it), and we
// have no live Didit credentials. This mirrors the precedent already set in
// issue_applications_test.go for the equivalent GitHub-API-shaped gap. What
// IS fully covered instead: auth, the two distinct "not configured" guards,
// and (for Status()) every DB-only code path - which is all of Status()
// except the "refresh from a live Didit session" branch, itself gated on
// kyc_session_id being non-NULL and therefore easy to avoid entirely by
// seeding kyc_session_id = NULL.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// kycSuiteJWTSecret mints/validates tokens for every test in this file.
const kycSuiteJWTSecret = "kyc-suite-test-jwt-secret"

// kycSuiteApp wires a fiber app exposing exactly the /auth/kyc/* routes
// internal/api/api.go registers against handlers.KYCHandler, including the
// same auth.RequireAuth middleware production uses.
func kycSuiteApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewKYCHandler(cfg, d, nil)
	app := fiber.New()
	app.Post("/auth/kyc/start", auth.RequireAuth(cfg.JWTSecret), h.Start())
	app.Get("/auth/kyc/status", auth.RequireAuth(cfg.JWTSecret), h.Status())
	return app
}

// kycSuiteJWT issues a real HS256 JWT via internal/auth's own issuing
// function, matching exactly what auth.RequireAuth expects.
func kycSuiteJWT(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(kycSuiteJWTSecret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("kycSuiteJWT: issue jwt: %v", err)
	}
	return tok
}

// kycSuiteInsertUser inserts a uniquely-identified user row with the given
// kyc_status (nil for SQL NULL) and kyc_session_id left NULL, so Status()
// never attempts a live Didit API call for it. Returns the new user id.
func kycSuiteInsertUser(t *testing.T, pool db.DBPool, kycStatus *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id, kyc_status)
VALUES ('contributor', $1, $2, $3)
RETURNING id
`, "kyc-suite-user-"+uuid.NewString(), projectsFxNextGHUserID(), kycStatus).Scan(&id)
	if err != nil {
		t.Fatalf("kycSuiteInsertUser: %v", err)
	}
	return id
}

func kycSuiteStrPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Status()
// ---------------------------------------------------------------------------

func TestKYCHandler_Status(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: kycSuiteJWTSecret} // DiditAPIKey deliberately unset
	app := kycSuiteApp(cfg, d)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/auth/kyc/status", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("not_started status", func(t *testing.T) {
		userID := kycSuiteInsertUser(t, d.Pool, kycSuiteStrPtr("not_started"))
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/auth/kyc/status", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["status"] != "not_started" {
			t.Errorf("status = %v, want \"not_started\"", resp["status"])
		}
		if resp["session_id"] != nil {
			t.Errorf("session_id = %v, want nil", resp["session_id"])
		}
	})

	t.Run("verified status", func(t *testing.T) {
		userID := kycSuiteInsertUser(t, d.Pool, kycSuiteStrPtr("verified"))
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/auth/kyc/status", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["status"] != "verified" {
			t.Errorf("status = %v, want \"verified\"", resp["status"])
		}
		if _, hasRejection := resp["rejection_reason"]; hasRejection {
			t.Errorf("expected no rejection_reason key for a verified user, got %v", resp["rejection_reason"])
		}
	})

	t.Run("rejected status with no stored decision data falls back to a generic rejection reason", func(t *testing.T) {
		userID := kycSuiteInsertUser(t, d.Pool, kycSuiteStrPtr("rejected"))
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/auth/kyc/status", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["status"] != "rejected" {
			t.Errorf("status = %v, want \"rejected\"", resp["status"])
		}
		if resp["rejection_reason"] != "Verification declined" {
			t.Errorf("rejection_reason = %v, want \"Verification declined\" (generic fallback when kyc_data carries no warnings)", resp["rejection_reason"])
		}
	})

	t.Run("user who never touched KYC (NULL kyc_status) returns a null status, not an error", func(t *testing.T) {
		userID := kycSuiteInsertUser(t, d.Pool, nil)
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "GET", "/auth/kyc/status", token, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["status"] != nil {
			t.Errorf("status = %v, want nil", resp["status"])
		}
		if resp["session_id"] != nil {
			t.Errorf("session_id = %v, want nil", resp["session_id"])
		}
		if resp["verified_at"] != nil {
			t.Errorf("verified_at = %v, want nil", resp["verified_at"])
		}
	})
}

// ---------------------------------------------------------------------------
// Start()
// ---------------------------------------------------------------------------

func TestKYCHandler_Start(t *testing.T) {
	d := testDB(t)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		cfg := config.Config{JWTSecret: kycSuiteJWTSecret}
		app := kycSuiteApp(cfg, d)
		status, _ := projectsFxDoJSON(t, app, "POST", "/auth/kyc/start", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("no Didit API key configured returns 503 kyc_not_configured", func(t *testing.T) {
		cfg := config.Config{JWTSecret: kycSuiteJWTSecret} // DiditAPIKey/DiditWorkflowID both unset
		app := kycSuiteApp(cfg, d)
		userID := kycSuiteInsertUser(t, d.Pool, nil)
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "POST", "/auth/kyc/start", token, nil)
		if status != fiber.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["error"] != "kyc_not_configured" {
			t.Errorf("error = %v, want kyc_not_configured", resp["error"])
		}
	})

	t.Run("Didit API key set but no workflow id still returns 503 kyc_not_configured", func(t *testing.T) {
		// NewKYCHandler only constructs a non-nil *didit.Client when
		// DiditAPIKey != "" (kyc.go:104-107); the handler then separately
		// requires DiditWorkflowID != "" (kyc.go:124-126). This exercises
		// the second guard specifically. No live Didit account is contacted
		// - the request never gets past this in-process config check.
		cfg := config.Config{JWTSecret: kycSuiteJWTSecret, DiditAPIKey: "kyc-suite-fake-api-key"}
		app := kycSuiteApp(cfg, d)
		userID := kycSuiteInsertUser(t, d.Pool, nil)
		token := kycSuiteJWT(t, userID, "contributor")

		status, body := projectsFxDoJSON(t, app, "POST", "/auth/kyc/start", token, nil)
		if status != fiber.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["error"] != "kyc_not_configured" {
			t.Errorf("error = %v, want kyc_not_configured", resp["error"])
		}
		if msg, _ := resp["message"].(string); msg == "" {
			t.Errorf("expected a non-empty message explaining DIDIT_WORKFLOW_ID is missing")
		}
	})
}
