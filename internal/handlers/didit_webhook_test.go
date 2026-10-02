package handlers_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// diditWebhookSuiteSecret is the shared webhook secret used by every test in
// this file that needs a validly-signed request.
const diditWebhookSuiteSecret = "didit-webhook-suite-test-secret"

// diditWebhookSuiteSignedHeaders computes the X-Signature/X-Timestamp header
// pair Receive() now requires on POST, matching Didit's documented
// HMAC-SHA256-over-raw-body scheme.
func diditWebhookSuiteSignedHeaders(secret string, body []byte, timestamp time.Time) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return map[string]string{
		"X-Signature": hex.EncodeToString(mac.Sum(nil)),
		"X-Timestamp": strconv.FormatInt(timestamp.Unix(), 10),
	}
}

// ---------------------------------------------------------------------------
// Shared helpers for the Didit (KYC) webhook black-box suite
// (didit_webhook_test.go). Everything here is prefixed with
// diditWebhookSuite to stay unique across the concurrently-written test
// files in this package (other agents own auth-core, oauth/github-app,
// projects/issues, admin).
// ---------------------------------------------------------------------------

// diditWebhookSuiteApp mounts GET/POST /webhooks/didit exactly as
// internal/api/api.go wires DiditWebhookHandler.
func diditWebhookSuiteApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewDiditWebhookHandler(cfg, d, nil)
	app.Get("/webhooks/didit", h.Receive())
	app.Post("/webhooks/didit", h.Receive())
	return app
}

// diditWebhookSuiteInsertUser inserts a uniquely-identified user row with
// the given kyc_session_id/kyc_status directly via SQL and returns its id.
func diditWebhookSuiteInsertUser(t *testing.T, d *db.DB, sessionID, kycStatus string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (display_name, kyc_session_id, kyc_status)
VALUES ($1, $2, $3)
RETURNING id
`, "didit-webhook-suite-user-"+uuid.NewString(), sessionID, kycStatus).Scan(&id)
	if err != nil {
		t.Fatalf("insert didit-webhook-suite test user: %v", err)
	}
	return id
}

// diditWebhookSuiteUserRow is the subset of a users row this suite asserts
// on after a webhook request has been processed.
type diditWebhookSuiteUserRow struct {
	KYCStatus     string
	KYCVerifiedAt *time.Time
}

// diditWebhookSuiteReadUser reads back kyc_status/kyc_verified_at for userID
// so tests can assert a webhook request actually persisted (or didn't).
func diditWebhookSuiteReadUser(t *testing.T, d *db.DB, userID uuid.UUID) diditWebhookSuiteUserRow {
	t.Helper()
	var row diditWebhookSuiteUserRow
	var status *string
	if err := d.Pool.QueryRow(context.Background(), `
SELECT kyc_status, kyc_verified_at FROM users WHERE id = $1
`, userID).Scan(&status, &row.KYCVerifiedAt); err != nil {
		t.Fatalf("read back didit-webhook-suite user: %v", err)
	}
	if status != nil {
		row.KYCStatus = *status
	}
	return row
}

// diditWebhookSuiteDo issues an HTTP request against app, disabling Fiber's
// default 1s test timeout since some of these tests hit a real Postgres
// instance shared with other concurrently-running test suites.
func diditWebhookSuiteDo(t *testing.T, app *fiber.App, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

// diditWebhookSuiteAssertError decodes body as {"error": "..."} and checks
// it matches want.
func diditWebhookSuiteAssertError(t *testing.T, body []byte, want string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if decoded["error"] != want {
		t.Errorf("error = %v, want %q", decoded["error"], want)
	}
}

// ---------------------------------------------------------------------------
// db_not_configured guard (no live DB required for this one)
// ---------------------------------------------------------------------------

func TestDiditWebhookReceive_NilDBServiceUnavailable(t *testing.T) {
	cases := []struct {
		name string
		d    *db.DB
	}{
		{"nil *db.DB", nil},
		{"non-nil *db.DB with nil Pool", &db.DB{Pool: nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := diditWebhookSuiteApp(config.Config{}, tc.d)

			t.Run("GET", func(t *testing.T) {
				resp, body := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit?verificationSessionId=whatever", nil, nil)
				if resp.StatusCode != fiber.StatusServiceUnavailable {
					t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusServiceUnavailable, body)
				}
				diditWebhookSuiteAssertError(t, body, "db_not_configured")
			})

			t.Run("POST", func(t *testing.T) {
				payload := []byte(`{"session_id":"whatever","status":"approved"}`)
				resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, nil)
				if resp.StatusCode != fiber.StatusServiceUnavailable {
					t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusServiceUnavailable, body)
				}
				diditWebhookSuiteAssertError(t, body, "db_not_configured")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Tests requiring a live DB
// ---------------------------------------------------------------------------

func TestDiditWebhookReceive_MissingSessionID(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
	app := diditWebhookSuiteApp(cfg, d)

	t.Run("GET without any session id query params", func(t *testing.T) {
		resp, body := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit", nil, nil)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusBadRequest, body)
		}
		diditWebhookSuiteAssertError(t, body, "missing_session_id")
	})

	t.Run("POST without session_id field", func(t *testing.T) {
		payload := []byte(`{"event":"status.updated","status":"approved"}`)
		headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, time.Now())
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusBadRequest, body)
		}
		diditWebhookSuiteAssertError(t, body, "missing_session_id")
	})
}

func TestDiditWebhookReceive_SessionNotFound(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
	app := diditWebhookSuiteApp(cfg, d)
	unknownSessionID := "unknown-session-" + uuid.NewString()

	t.Run("GET", func(t *testing.T) {
		resp, body := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+unknownSessionID, nil, nil)
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusNotFound, body)
		}
		diditWebhookSuiteAssertError(t, body, "session_not_found")
	})

	t.Run("POST", func(t *testing.T) {
		payload := []byte(fmt.Sprintf(`{"session_id":%q,"status":"approved"}`, unknownSessionID))
		headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, time.Now())
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusNotFound, body)
		}
		diditWebhookSuiteAssertError(t, body, "session_not_found")
	})
}

func TestDiditWebhookReceive_POSTMalformedJSONBadRequest(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
	app := diditWebhookSuiteApp(cfg, d)

	payload := []byte(`{not-valid-json`)
	headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, time.Now())
	resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusBadRequest, body)
	}
	diditWebhookSuiteAssertError(t, body, "invalid_json")
}

// Without a Didit client the handler has no way to learn the real decision,
// so a correctly signed delivery changes nothing and is answered 5xx for Didit
// to redeliver. Both of these used to assert the opposite - that the body's
// status was applied as-is - which is the fallback that let an unconfirmed
// status through. The positive path (the decision fetched from Didit's API is
// what gets written) is covered in didit_webhook_trust_test.go, which can
// stand in a client for Didit.
func TestDiditWebhookReceive_POSTWithoutDiditClientLeavesStatusUnchanged(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
	app := diditWebhookSuiteApp(cfg, d)

	for _, status := range []string{"approved", "declined"} {
		t.Run(status, func(t *testing.T) {
			sessionID := "sess-noclient-" + uuid.NewString()
			userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

			payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":%q}`, sessionID, status))
			headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, time.Now())
			resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
			if resp.StatusCode != fiber.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusServiceUnavailable, body)
			}
			diditWebhookSuiteAssertError(t, body, "didit_not_configured")

			row := diditWebhookSuiteReadUser(t, d, userID)
			if row.KYCStatus != "pending" {
				t.Errorf("kyc_status = %q, want unchanged %q", row.KYCStatus, "pending")
			}
			if row.KYCVerifiedAt != nil {
				t.Errorf("kyc_verified_at = %v, want nil", *row.KYCVerifiedAt)
			}
		})
	}
}

// The browser callback still lands the person back in the app, but the
// status in its query string is never applied, and the redirect reports the
// status we actually hold rather than claiming "verified".
func TestDiditWebhookReceive_GETRedirectsWhenFrontendBaseURLConfigured(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{FrontendBaseURL: "https://app.example.com/"}
	app := diditWebhookSuiteApp(cfg, d)

	sessionID := "sess-redirect-" + uuid.NewString()
	userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

	resp, _ := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+sessionID+"&status=approved", nil, nil)
	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusFound)
	}
	wantLocation := "https://app.example.com?kyc=pending&session_id=" + sessionID
	if got := resp.Header.Get("Location"); got != wantLocation {
		t.Errorf("Location = %q, want %q", got, wantLocation)
	}

	row := diditWebhookSuiteReadUser(t, d, userID)
	if row.KYCStatus != "pending" {
		t.Errorf("kyc_status = %q, want unchanged %q - the URL's status=approved must not be applied", row.KYCStatus, "pending")
	}
}

func TestDiditWebhookReceive_GETFallsBackToJSONWhenNoRedirectConfigured(t *testing.T) {
	d := testDB(t)
	// Deliberately no FrontendBaseURL / GitHubOAuthSuccessRedirectURL.
	app := diditWebhookSuiteApp(config.Config{}, d)

	sessionID := "sess-json-fallback-" + uuid.NewString()
	diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

	resp, body := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+sessionID+"&status=approved", nil, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (falls through to JSON when no redirect target is configured); body=%s", resp.StatusCode, fiber.StatusOK, body)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode response %s: %v", body, err)
	}
	if decoded["status"] != "pending" {
		t.Errorf("status = %v, want the stored %q, not the query's approved", decoded["status"], "pending")
	}
}

func TestDiditWebhookReceive_GETAlternateSessionIDQueryParam(t *testing.T) {
	d := testDB(t)
	app := diditWebhookSuiteApp(config.Config{}, d)

	sessionID := "sess-altparam-" + uuid.NewString()
	userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

	// Uses the "session_id" fallback query param instead of
	// "verificationSessionId" (didit_webhook.go's alternate-name lookup). A
	// 200 rather than session_not_found is the proof the lookup worked; the
	// status is untouched because there is no Didit client to confirm it.
	resp, body := diditWebhookSuiteDo(t, app, "GET", "/webhooks/didit?session_id="+sessionID+"&status=rejected", nil, nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusOK, body)
	}

	row := diditWebhookSuiteReadUser(t, d, userID)
	if row.KYCStatus != "pending" {
		t.Errorf("kyc_status = %q, want unchanged %q", row.KYCStatus, "pending")
	}
}

// TestDiditWebhookReceive_POSTSignatureVerification is a SECURITY
// REGRESSION SUITE for a real vulnerability found while writing this test
// file and fixed in didit_webhook.go (verifyDiditSignature): Receive() used
// to never read h.cfg.DiditWebhookSecret or validate any signature header at
// all, so anyone who learned/guessed a kyc_session_id could flip that user's
// KYC status with a single unauthenticated POST. It's fixed now - every
// scenario below must be rejected before the KYC status is ever touched.
func TestDiditWebhookReceive_POSTSignatureVerification(t *testing.T) {
	d := testDB(t)

	newSession := func(t *testing.T) (string, string) {
		t.Helper()
		sessionID := "sess-sig-" + uuid.NewString()
		return sessionID, sessionID
	}

	t.Run("no secret configured is refused, not silently permissive", func(t *testing.T) {
		app := diditWebhookSuiteApp(config.Config{}, d) // DiditWebhookSecret left empty
		sessionID, _ := newSession(t)
		userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, nil)
		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusServiceUnavailable, body)
		}
		diditWebhookSuiteAssertError(t, body, "webhook_secret_not_configured")

		row := diditWebhookSuiteReadUser(t, d, userID)
		if row.KYCStatus != "pending" {
			t.Errorf("kyc_status = %q, want unchanged %q", row.KYCStatus, "pending")
		}
	})

	t.Run("forged signature headers are rejected", func(t *testing.T) {
		cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
		app := diditWebhookSuiteApp(cfg, d)
		sessionID, _ := newSession(t)
		userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, map[string]string{
			"X-Signature": "0000000000000000000000000000000000000000000000000000000000000000",
			"X-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		})
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusUnauthorized, body)
		}
		diditWebhookSuiteAssertError(t, body, "invalid_signature")

		row := diditWebhookSuiteReadUser(t, d, userID)
		if row.KYCStatus != "pending" {
			t.Errorf("kyc_status = %q, want unchanged %q - a forged signature must not update it", row.KYCStatus, "pending")
		}
	})

	t.Run("missing signature headers entirely are rejected", func(t *testing.T) {
		cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
		app := diditWebhookSuiteApp(cfg, d)
		sessionID, _ := newSession(t)
		diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, nil)
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusUnauthorized, body)
		}
		diditWebhookSuiteAssertError(t, body, "invalid_signature")
	})

	t.Run("signature valid for a different body is rejected (tamper-evident)", func(t *testing.T) {
		cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
		app := diditWebhookSuiteApp(cfg, d)
		sessionID, _ := newSession(t)
		userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		signedPayload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"declined"}`, sessionID))
		headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, signedPayload, time.Now())

		// Send a DIFFERENT body than the one the signature actually covers.
		actualPayload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", actualPayload, headers)
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusUnauthorized, body)
		}

		row := diditWebhookSuiteReadUser(t, d, userID)
		if row.KYCStatus != "pending" {
			t.Errorf("kyc_status = %q, want unchanged %q", row.KYCStatus, "pending")
		}
	})

	t.Run("expired timestamp is rejected as a replay", func(t *testing.T) {
		cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
		app := diditWebhookSuiteApp(cfg, d)
		sessionID, _ := newSession(t)
		userID := diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		old := time.Now().Add(-10 * time.Minute) // well outside the 300s window
		headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, old)
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, fiber.StatusUnauthorized, body)
		}

		row := diditWebhookSuiteReadUser(t, d, userID)
		if row.KYCStatus != "pending" {
			t.Errorf("kyc_status = %q, want unchanged %q", row.KYCStatus, "pending")
		}
	})

	t.Run("correctly signed, fresh request gets past the signature check", func(t *testing.T) {
		cfg := config.Config{DiditWebhookSecret: diditWebhookSuiteSecret}
		app := diditWebhookSuiteApp(cfg, d)
		sessionID, _ := newSession(t)
		diditWebhookSuiteInsertUser(t, d, sessionID, "pending")

		// Accepted means "authenticated", not "applied": with no Didit
		// client the decision cannot be confirmed, so the answer is
		// didit_not_configured rather than invalid_signature. That the
		// confirmed decision is then written is covered in
		// didit_webhook_trust_test.go.
		payload := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"approved"}`, sessionID))
		headers := diditWebhookSuiteSignedHeaders(diditWebhookSuiteSecret, payload, time.Now())
		resp, body := diditWebhookSuiteDo(t, app, "POST", "/webhooks/didit", payload, headers)
		if resp.StatusCode == fiber.StatusUnauthorized {
			t.Fatalf("status = %d, a correctly signed request must not be rejected; body=%s", resp.StatusCode, body)
		}
		diditWebhookSuiteAssertError(t, body, "didit_not_configured")
	})
}
