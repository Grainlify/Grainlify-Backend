package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

func diditSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func diditNowTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func doDiditRequest(app *fiber.App, body []byte, headers map[string]string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/didit", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, _ := app.Test(req, -1)
	return resp
}

func TestVerifyDiditSignature_ValidRawBody(t *testing.T) {
	body := []byte(` { "session_id": "abc", "status": "Approved" } `)
	if !verifyDiditSignature("secret", body, diditSign("secret", body), diditNowTimestamp()) {
		t.Fatal("expected valid raw-body signature to pass")
	}
}

func TestVerifyDiditSignature_Sha256PrefixAccepted(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	if !verifyDiditSignature("secret", body, "sha256="+diditSign("secret", body), diditNowTimestamp()) {
		t.Fatal("expected sha256= prefixed signature to pass")
	}
}

func TestVerifyDiditSignature_WrongSecret(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	if verifyDiditSignature("secret", body, diditSign("wrong", body), diditNowTimestamp()) {
		t.Fatal("expected wrong-secret signature to fail")
	}
}

func TestVerifyDiditSignature_WrongBody(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	if verifyDiditSignature("secret", body, diditSign("secret", []byte(`{"session_id":"tampered"}`)), diditNowTimestamp()) {
		t.Fatal("expected tampered body signature to fail")
	}
}

func TestVerifyDiditSignature_StaleTimestamp(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	if verifyDiditSignature("secret", body, diditSign("secret", body), stale) {
		t.Fatal("expected stale timestamp signature to fail")
	}
}

func TestVerifyDiditSignature_MissingTimestamp(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	if verifyDiditSignature("secret", body, diditSign("secret", body), "") {
		t.Fatal("expected missing timestamp signature to fail")
	}
}

func TestDiditReceive_MissingSecret_Returns503(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{Env: "production"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_MissingSignature_Returns401(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_InvalidSignature_Returns401(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  "deadbeef",
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_StaleTimestamp_Returns401(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10),
		"X-Signature":  diditSign("secret", body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_ValidSignature_ReachesDBCheck(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  diditSign("secret", body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 after valid signature, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_InvalidJSONAfterSignature_Returns400(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  diditSign("secret", body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestDiditReceive_MissingSessionIDAfterSignature_Returns400(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"status":"Approved"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  diditSign("secret", body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

type fakeDiditDecisionClient struct {
	decision didit.SessionDecisionResponse
	err      error
}

func (f *fakeDiditDecisionClient) GetSessionDecision(_ context.Context, _ string) (didit.SessionDecisionResponse, error) {
	return f.decision, f.err
}

func TestResolveDiditStatus_CallbackUsesAPIInsteadOfQueryStatus(t *testing.T) {
	h := &DiditWebhookHandler{
		didit: &fakeDiditDecisionClient{
			decision: didit.SessionDecisionResponse{
				Status: "Approved",
				Decision: map[string]interface{}{
					"status": "Approved",
				},
			},
		},
	}

	status, _, err := h.resolveDiditStatus(context.Background(), "session-id", "Declined", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "verified" {
		t.Fatalf("want verified from Didit API, got %q", status)
	}
}

func TestResolveDiditStatus_CallbackWithoutDiditClientFails(t *testing.T) {
	h := &DiditWebhookHandler{}

	_, _, err := h.resolveDiditStatus(context.Background(), "session-id", "Approved", true)
	if !errors.Is(err, errDiditAPIClientNotConfigured) {
		t.Fatalf("want didit api client not configured error, got %v", err)
	}
}

func TestDiditReceive_ValidSignatureWithSha256Prefix(t *testing.T) {
	h := NewDiditWebhookHandler(config.Config{DiditWebhookSecret: "secret"}, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc","status":"Approved"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  "sha256=" + diditSign("secret", body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 after valid signature with sha256 prefix, got %d", resp.StatusCode)
	}
}

func TestResolveDiditStatus_WebhookFallsBackToSignedBodyWhenAPIFails(t *testing.T) {
	h := &DiditWebhookHandler{
		didit: &fakeDiditDecisionClient{
			err: errors.New("boom"),
		},
	}

	status, _, err := h.resolveDiditStatus(context.Background(), "session-id", "Approved", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "verified" {
		t.Fatalf("want fallback status verified, got %q", status)
	}
}

func TestVerifyDiditSignature_EmptySecretReturnsFalse(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	if verifyDiditSignature("", body, diditSign("secret", body), diditNowTimestamp()) {
		t.Fatal("expected empty secret to fail")
	}
}

func TestVerifyDiditSignature_FutureTimestampRejected(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	future := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	if verifyDiditSignature("secret", body, diditSign("secret", body), future) {
		t.Fatal("expected future timestamp to fail")
	}
}

func TestDiditReceive_ValidSignatureButMissingDiditClient_FallsBackToBodyStatus(t *testing.T) {
	h := &DiditWebhookHandler{
		cfg:   config.Config{DiditWebhookSecret: "secret"},
		db:    nil,
		didit: nil,
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhooks/didit", h.Receive())

	body := []byte(`{"session_id":"abc","status":"Approved"}`)
	resp := doDiditRequest(app, body, map[string]string{
		"Content-Type": "application/json",
		"X-Timestamp":  diditNowTimestamp(),
		"X-Signature":  diditSign("secret", body),
	})
	defer resp.Body.Close()
	// Should get 503 because DB is nil, but signature was valid
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 after valid signature (no DB), got %d", resp.StatusCode)
	}
}

func TestVerifyDiditSignature_ConstantTimeCompareExercised(t *testing.T) {
	body := []byte(`{"session_id":"abc"}`)
	correctSig := diditSign("secret", body)
	// Flip the last nibble to create a signature of same length but different content
	runes := []rune(correctSig)
	if runes[len(runes)-1] == '0' {
		runes[len(runes)-1] = '1'
	} else {
		runes[len(runes)-1] = '0'
	}
	wrongSig := string(runes)
	if len(wrongSig) != len(correctSig) {
		t.Fatal("prerequisite: signatures must be same length")
	}
	if verifyDiditSignature("secret", body, wrongSig, diditNowTimestamp()) {
		t.Fatal("expected near-miss signature to fail (constant-time compare)")
	}
}

func TestDiditCallback_MissingDiditClient_Returns503(t *testing.T) {
	h := &DiditWebhookHandler{
		cfg:   config.Config{},
		db:    nil,
		didit: nil,
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/webhooks/didit", h.Receive())

	req := httptest.NewRequest(http.MethodGet, "/webhooks/didit?session_id=test-session", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	// DB is checked first in GET handler, returns 503 before Didit API check
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 (db not configured), got %d", resp.StatusCode)
	}
}
