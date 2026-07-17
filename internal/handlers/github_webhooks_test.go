package handlers

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
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// mockBus is a thread-safe in-memory bus.Bus for testing.
// It records every Publish call so tests can assert publication behaviour.
type mockBus struct {
	mu   sync.Mutex
	msgs []mockMsg
}

type mockMsg struct {
	subject string
	data    []byte
}

func (b *mockBus) Publish(_ context.Context, subject string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, mockMsg{subject: subject, data: data})
	return nil
}

func (b *mockBus) Status() string { return "CONNECTED" }

func (b *mockBus) Close() {}

func (b *mockBus) published() []mockMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]mockMsg, len(b.msgs))
	copy(out, b.msgs)
	return out
}

// sign computes the sha256= HMAC header value as GitHub does.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// verifyGitHubSignature unit tests
// ---------------------------------------------------------------------------

func TestVerifyGitHubSignature_Valid(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := sign("mysecret", body)
	if !verifyGitHubSignature("mysecret", body, sig) {
		t.Fatal("expected valid signature to pass")
	}
}

func TestVerifyGitHubSignature_WrongSecret(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := sign("wrongsecret", body)
	// The function uses constant-time compare, but result must still be false.
	if verifyGitHubSignature("mysecret", body, sig) {
		t.Fatal("expected wrong-secret signature to fail")
	}
}

func TestVerifyGitHubSignature_WrongBody(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := sign("mysecret", []byte(`{"action":"tampered"}`))
	if verifyGitHubSignature("mysecret", body, sig) {
		t.Fatal("expected tampered-body signature to fail")
	}
}

func TestVerifyGitHubSignature_MissingPrefix(t *testing.T) {
	body := []byte(`{}`)
	mac := hmac.New(sha256.New, []byte("mysecret"))
	_, _ = mac.Write(body)
	// header without "sha256=" prefix
	bare := hex.EncodeToString(mac.Sum(nil))
	if verifyGitHubSignature("mysecret", body, bare) {
		t.Fatal("expected missing sha256= prefix to fail")
	}
}

func TestVerifyGitHubSignature_Sha1Only(t *testing.T) {
	body := []byte(`{}`)
	mac := hmac.New(sha256.New, []byte("mysecret"))
	_, _ = mac.Write(body)
	// Simulate an sha1= header (old format – must NOT be accepted)
	sha1Header := "sha1=" + hex.EncodeToString(mac.Sum(nil))
	if verifyGitHubSignature("mysecret", body, sha1Header) {
		t.Fatal("expected sha1= prefix to fail; only sha256= is accepted")
	}
}

func TestVerifyGitHubSignature_EmptyHeader(t *testing.T) {
	if verifyGitHubSignature("mysecret", []byte(`{}`), "") {
		t.Fatal("expected empty header to fail")
	}
}

// assertConstantTimeCompare verifies that verifyGitHubSignature still returns
// false even when the hex strings are equal length (i.e., it exercises the
// subtle.ConstantTimeCompare branch rather than short-circuiting on length).
func TestVerifyGitHubSignature_ConstantTimeCompareExercised(t *testing.T) {
	body := []byte(`{"action":"ping"}`)
	// Construct a header that has the right prefix and right length, but wrong value.
	correctSig := sign("secret", body)
	// Flip the last nibble.
	runes := []rune(correctSig)
	if runes[len(runes)-1] == '0' {
		runes[len(runes)-1] = '1'
	} else {
		runes[len(runes)-1] = '0'
	}
	wrongSig := string(runes)
	// Both are 71 chars ("sha256=" + 64 hex chars) — same length, different content.
	if len(wrongSig) != len(correctSig) {
		t.Fatal("prerequisite: signatures must be the same length for this test to be meaningful")
	}
	if verifyGitHubSignature("secret", body, wrongSig) {
		t.Fatal("expected near-miss signature to fail (constant-time compare)")
	}
}

// ---------------------------------------------------------------------------
// Receive() handler tests
// ---------------------------------------------------------------------------

// newTestApp wires the handler into a minimal Fiber app.
func newTestApp(h *GitHubWebhooksHandler) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/webhook", h.Receive())
	return app
}

func doRequest(app *fiber.App, body []byte, headers map[string]string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, _ := app.Test(req, -1)
	return resp
}

func TestReceive_MissingSecret_Returns503(t *testing.T) {
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: ""}, nil, nil)
	app := newTestApp(h)

	body := []byte(`{"action":"ping"}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":      "application/json",
		"X-GitHub-Event":    "ping",
		"X-GitHub-Delivery": "abc-1",
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

func TestReceive_InvalidSignature_Returns401(t *testing.T) {
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "secret"}, nil, nil)
	app := newTestApp(h)

	body := []byte(`{"action":"ping"}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "abc-2",
		"X-Hub-Signature-256": "sha256=deadbeef",
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestReceive_ValidSignature_PublishesToBus(t *testing.T) {
	bus := &mockBus{}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "secret"}, nil, bus)
	app := newTestApp(h)

	body := []byte(`{"action":"opened","repository":{"full_name":"acme/widget"}}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "del-123",
		"X-Hub-Signature-256": sign("secret", body),
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	msgs := bus.published()
	if len(msgs) != 1 {
		t.Fatalf("want 1 published message, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].subject, "github.") {
		t.Fatalf("unexpected subject %q", msgs[0].subject)
	}

	// Assert the published payload deserializes with the correct fields.
	var ev map[string]any
	if err := json.Unmarshal(msgs[0].data, &ev); err != nil {
		t.Fatalf("published data not valid JSON: %v", err)
	}
	if ev["delivery_id"] != "del-123" {
		t.Errorf("delivery_id mismatch: %v", ev["delivery_id"])
	}
	if ev["event"] != "issues" {
		t.Errorf("event mismatch: %v", ev["event"])
	}
}

func TestReceive_Sha1SignatureRejected(t *testing.T) {
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "secret"}, nil, nil)
	app := newTestApp(h)

	body := []byte(`{"action":"ping"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	sha1Header := fmt.Sprintf("sha1=%s", hex.EncodeToString(mac.Sum(nil)))

	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "abc-3",
		"X-Hub-Signature-256": sha1Header, // sha1= prefix instead of sha256=
	})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 for sha1-only header, got %d", resp.StatusCode)
	}
}

func TestReceive_ValidSignature_InlinesWhenNoBus(t *testing.T) {
	// With no bus and no DB (ingestor will be nil), Receive must still return 200.
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "s"}, nil, nil)
	app := newTestApp(h)

	body := []byte(`{"action":"ping"}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "abc-4",
		"X-Hub-Signature-256": sign("s", body),
	})
	defer io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestReceive_OptionsPreflightReturns200(t *testing.T) {
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "s"}, nil, nil)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Options("/webhook", h.Receive())

	req := httptest.NewRequest(http.MethodOptions, "/webhook", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200 for OPTIONS, got %d", resp.StatusCode)
	}
}
