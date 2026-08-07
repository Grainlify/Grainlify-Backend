package handlers_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/events"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Shared helpers for the GitHub webhooks black-box suite
// (github_webhooks_test.go). Everything here is prefixed with ghWebhookSuite
// (or fakeWebhookBus, per naming guidance) to stay unique across the
// concurrently-written test files in this package (other agents own
// auth-core, oauth/github-app, projects/issues, admin).
// ---------------------------------------------------------------------------

// ghWebhookSuiteSecret is the fixed webhook secret used to sign requests in
// this suite.
const ghWebhookSuiteSecret = "gh-webhook-suite-test-secret"

// fakeWebhookBusMsg records a single call to fakeWebhookBus.Publish.
type fakeWebhookBusMsg struct {
	subject string
	data    []byte
}

// fakeWebhookBus is a minimal in-memory bus.Bus implementation that records
// every published message, so tests can assert what the GitHub webhook
// handler published without needing a real NATS connection.
type fakeWebhookBus struct {
	mu         sync.Mutex
	published  []fakeWebhookBusMsg
	publishErr error
}

func (b *fakeWebhookBus) Publish(ctx context.Context, subject string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishErr != nil {
		return b.publishErr
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.published = append(b.published, fakeWebhookBusMsg{subject: subject, data: cp})
	return nil
}

func (b *fakeWebhookBus) Close() {}

func (b *fakeWebhookBus) messages() []fakeWebhookBusMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]fakeWebhookBusMsg, len(b.published))
	copy(out, b.published)
	return out
}

// Compile-time assertion that fakeWebhookBus satisfies bus.Bus.
var _ bus.Bus = (*fakeWebhookBus)(nil)

// ghWebhookSuiteSign computes the GitHub-style X-Hub-Signature-256 header
// value for body under secret, independently of the handler's own
// verifyGitHubSignature/hexEncodeLower implementation (which lives in the
// white-box github_webhooks_internal_test.go and is not reachable from this
// black-box _test package).
func ghWebhookSuiteSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ghWebhookSuiteApp mounts POST /webhooks/github exactly as
// internal/api/api.go wires GitHubWebhooksHandler.
func ghWebhookSuiteApp(cfg config.Config, b bus.Bus) *fiber.App {
	app := fiber.New()
	h := handlers.NewGitHubWebhooksHandler(cfg, nil, b, nil)
	app.Post("/webhooks/github", h.Receive())
	return app
}

// ghWebhookSuiteDo issues a POST /webhooks/github request with the given
// body and headers, returning the status code and raw response body.
func ghWebhookSuiteDo(t *testing.T, app *fiber.App, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
	return resp.StatusCode, respBody
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGitHubWebhooksReceive_ValidSignaturePublishesToBus(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"action":"opened","repository":{"full_name":"octocat/Hello-World"}}`)
	sig := ghWebhookSuiteSign(ghWebhookSuiteSecret, payload)

	status, _ := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "delivery-1",
	})

	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
	}

	msgs := fb.messages()
	if len(msgs) != 1 {
		t.Fatalf("published messages = %d, want 1", len(msgs))
	}
	if msgs[0].subject != events.SubjectGitHubWebhookReceived {
		t.Errorf("subject = %q, want %q", msgs[0].subject, events.SubjectGitHubWebhookReceived)
	}

	var ev events.GitHubWebhookReceived
	if err := json.Unmarshal(msgs[0].data, &ev); err != nil {
		t.Fatalf("unmarshal published event: %v", err)
	}
	if ev.DeliveryID != "delivery-1" {
		t.Errorf("DeliveryID = %q, want %q", ev.DeliveryID, "delivery-1")
	}
	if ev.Event != "pull_request" {
		t.Errorf("Event = %q, want %q", ev.Event, "pull_request")
	}
	if ev.Action != "opened" {
		t.Errorf("Action = %q, want %q", ev.Action, "opened")
	}
	if ev.RepoFullName != "octocat/Hello-World" {
		t.Errorf("RepoFullName = %q, want %q", ev.RepoFullName, "octocat/Hello-World")
	}
	if string(ev.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", ev.Payload, payload)
	}
}

func TestGitHubWebhooksReceive_InvalidSignatureRejected(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"action":"opened","repository":{"full_name":"octocat/Hello-World"}}`)
	wrongSig := ghWebhookSuiteSign("a-totally-different-secret", payload)

	status, body := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": wrongSig,
		"X-GitHub-Event":      "pull_request",
	})

	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", status, fiber.StatusUnauthorized, body)
	}
	if len(fb.messages()) != 0 {
		t.Errorf("published messages = %d, want 0 for a rejected request", len(fb.messages()))
	}
}

func TestGitHubWebhooksReceive_MissingSignatureHeaderRejected(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"action":"opened","repository":{"full_name":"octocat/Hello-World"}}`)

	// Deliberately no X-Hub-Signature-256 header at all.
	status, body := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-GitHub-Event": "pull_request",
	})

	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", status, fiber.StatusUnauthorized, body)
	}
	if len(fb.messages()) != 0 {
		t.Errorf("published messages = %d, want 0 for a request with no signature header", len(fb.messages()))
	}
}

func TestGitHubWebhooksReceive_EmptySignatureHeaderRejected(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"action":"opened"}`)

	status, _ := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": "",
		"X-GitHub-Event":      "pull_request",
	})

	if status != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, fiber.StatusUnauthorized)
	}
}

// TestGitHubWebhooksReceive_MalformedJSONBodyReturns200ButSilentlyDropsEvent
// documents real (surprising, and arguably buggy) behavior discovered by
// this test - it is NOT the naive "400 Bad Request" one might expect:
//
//  1. The signature is computed over raw bytes and verifies fine regardless
//     of JSON validity, and Receive() only populates action/repo "if err ==
//     nil" from json.Unmarshal (github_webhooks.go around line 127) - it
//     never inspects that error to reject the request. So far so 200.
//  2. events.GitHubWebhookReceived.Payload is typed json.RawMessage
//     (internal/events/types.go:14). When Receive() calls json.Marshal(ev)
//     to publish to the bus (github_webhooks.go:156), encoding/json
//     compacts/validates that raw message and FAILS if it isn't valid JSON
//     ("json: error calling MarshalJSON for type json.RawMessage: unexpected
//     end of JSON input" for a truncated body).
//  3. That marshal error is only logged (github_webhooks.go:157-161) - it is
//     never surfaced to the caller, and h.bus.Publish is never called for
//     this request. Execution falls through to the unconditional
//     `return c.SendStatus(fiber.StatusOK)` at github_webhooks.go:180.
//
// Net effect: a correctly-signed webhook delivery with a malformed JSON body
// gets HTTP 200 (telling GitHub "delivered, don't retry") while the event is
// silently and permanently dropped - never published to the bus, never
// ingested, with only a log line as a trace. See final report for file:line.
func TestGitHubWebhooksReceive_MalformedJSONBodyReturns200ButSilentlyDropsEvent(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"action": "opened", "repository": `) // truncated / invalid JSON
	sig := ghWebhookSuiteSign(ghWebhookSuiteSecret, payload)

	status, body := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "pull_request",
	})

	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (handler does not validate JSON structure, only the signature); body=%s", status, fiber.StatusOK, body)
	}

	// The interesting part: despite the 200, nothing was actually published -
	// json.Marshal(ev) failed on the malformed Payload and the error was
	// only logged. This documents a silent data-loss bug, not a pass/fail
	// judgment call by this test.
	msgs := fb.messages()
	if len(msgs) != 0 {
		t.Fatalf("published messages = %d, want 0 (malformed JSON payload should fail json.Marshal(ev) and never reach Publish - if this now fails, the marshal bug may have been fixed, which would be good news worth updating this test for)", len(msgs))
	}
}

// TestGitHubWebhooksReceive_UnrecognizedEventTypeStillAccepted documents that
// Receive() never branches on X-GitHub-Event: any correctly-signed request is
// forwarded to the bus regardless of event type, recognized or not. There is
// no event-type allowlist/denylist in this handler; any such filtering would
// need to happen downstream (e.g. in a NATS subscriber reading
// events.GitHubWebhookReceived.Event).
func TestGitHubWebhooksReceive_UnrecognizedEventTypeStillAccepted(t *testing.T) {
	cfg := config.Config{GitHubWebhookSecret: ghWebhookSuiteSecret}
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{"zen":"Responsive is better than fast."}`)
	sig := ghWebhookSuiteSign(ghWebhookSuiteSecret, payload)

	status, body := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": sig,
		"X-GitHub-Event":      "some_totally_unrecognized_event_type",
	})

	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", status, fiber.StatusOK, body)
	}

	msgs := fb.messages()
	if len(msgs) != 1 {
		t.Fatalf("published messages = %d, want 1", len(msgs))
	}
	var ev events.GitHubWebhookReceived
	if err := json.Unmarshal(msgs[0].data, &ev); err != nil {
		t.Fatalf("unmarshal published event: %v", err)
	}
	if ev.Event != "some_totally_unrecognized_event_type" {
		t.Errorf("Event = %q, want %q", ev.Event, "some_totally_unrecognized_event_type")
	}
}

func TestGitHubWebhooksReceive_SecretNotConfiguredRejectsAllRequests(t *testing.T) {
	cfg := config.Config{} // GitHubWebhookSecret intentionally left empty
	fb := &fakeWebhookBus{}
	app := ghWebhookSuiteApp(cfg, fb)

	payload := []byte(`{}`)
	status, _ := ghWebhookSuiteDo(t, app, payload, map[string]string{
		"X-Hub-Signature-256": "sha256=deadbeef",
	})

	if status != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, fiber.StatusServiceUnavailable)
	}
	if len(fb.messages()) != 0 {
		t.Errorf("published messages = %d, want 0 when webhook secret isn't configured", len(fb.messages()))
	}
}
