package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/events"
)

// mockBus is a thread-safe in-memory bus.Bus for testing.
// It records every Publish call so tests can assert publication behaviour.
type mockBus struct {
	mu         sync.Mutex
	msgs       []mockMsg
	publishErr error
}

type mockMsg struct {
	subject string
	data    []byte
}

func (b *mockBus) Publish(_ context.Context, subject string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishErr != nil {
		return b.publishErr
	}
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

type stubGitHubWebhookIngestor struct {
	err   error
	calls int
}

func (i *stubGitHubWebhookIngestor) Ingest(context.Context, events.GitHubWebhookReceived) error {
	i.calls++
	return i.err
}

type failingWebhookPool struct {
	err error
}

type failingWebhookRow struct {
	err error
}

func (r failingWebhookRow) Scan(...any) error { return r.err }

func (p failingWebhookPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, p.err
}

func (p failingWebhookPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, p.err
}

func (p failingWebhookPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingWebhookRow{err: p.err}
}

func (p failingWebhookPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, p.err
}

func (p failingWebhookPool) Ping(context.Context) error { return p.err }

func (p failingWebhookPool) Close() {}

func (p failingWebhookPool) Config() *pgxpool.Config { return nil }

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
// false even when the decoded signatures are equal length, exercising the
// hmac.Equal comparison path rather than only malformed-input rejection.
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

func TestReceive_NATSPublishFailure_Returns503(t *testing.T) {
	bus := &mockBus{publishErr: errors.New("nats unavailable")}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "secret"}, nil, bus)
	app := newTestApp(h)

	body := []byte(`{"action":"opened","repository":{"full_name":"acme/widget"}}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "del-retry-nats",
		"X-Hub-Signature-256": sign("secret", body),
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 so GitHub retries, got %d", resp.StatusCode)
	}
}

func TestReceive_NATSMarshalFailure_Returns500(t *testing.T) {
	bus := &mockBus{}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "secret"}, nil, bus)
	app := newTestApp(h)

	body := []byte(`{`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "del-retry-marshal",
		"X-Hub-Signature-256": sign("secret", body),
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("want 500 so GitHub can redeliver, got %d", resp.StatusCode)
	}
	if got := len(bus.published()); got != 0 {
		t.Fatalf("want no NATS publish after marshal failure, got %d", got)
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

func TestReceive_InlineIngestFailure_Returns503(t *testing.T) {
	pool := failingWebhookPool{err: errors.New("database unavailable")}
	h := NewGitHubWebhooksHandler(
		config.Config{GitHubWebhookSecret: "s"},
		&db.DB{Pool: pool},
		nil,
	)
	app := newTestApp(h)

	body := []byte(`{"action":"opened","repository":{"full_name":"acme/widget"}}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "del-retry-inline",
		"X-Hub-Signature-256": sign("s", body),
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("want 503 so GitHub retries, got %d", resp.StatusCode)
	}
}

func TestReceive_InlineIngestSuccess_Returns200(t *testing.T) {
	ingestor := &stubGitHubWebhookIngestor{}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: "s"}, nil, nil)
	h.ing = ingestor
	app := newTestApp(h)

	body := []byte(`{"action":"opened","repository":{"full_name":"acme/widget"}}`)
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "del-inline-success",
		"X-Hub-Signature-256": sign("s", body),
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ingestor.calls != 1 {
		t.Fatalf("want 1 inline ingest call, got %d", ingestor.calls)
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

// ---------------------------------------------------------------------------
// trackingIngestor — in-memory dedup mirroring GitHubWebhookIngestor behaviour
// ---------------------------------------------------------------------------
//
// trackingIngestor is a drop-in replacement for the real GitHubWebhookIngestor
// that records every delivery ID it sees and silently drops duplicates —
// exactly what the real ingestor does via the webhook_delivery_dedup table.
// Using it lets us prove the handler wires up and calls the ingestor correctly
// without requiring a live database.
//
// Thread-safety: all fields are protected by mu so tests can drive concurrent
// deliveries safely.
type trackingIngestor struct {
	mu      sync.Mutex
	seen    map[string]int // delivery_id → call count (first call + any replays)
	ingested []events.GitHubWebhookReceived
	err     error // if non-nil, returned on every call (simulates DB failure)
}

func newTrackingIngestor() *trackingIngestor {
	return &trackingIngestor{seen: make(map[string]int)}
}

// Ingest mirrors the real ingestor: the first call for a delivery ID is
// accepted and recorded; subsequent calls for the same ID are silently
// dropped (return nil, no side-effects).  This lets tests assert that
// replay deliveries produce exactly one logical ingest regardless of how
// many times the HTTP endpoint is hit.
func (ti *trackingIngestor) Ingest(_ context.Context, ev events.GitHubWebhookReceived) error {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	if ti.err != nil {
		return ti.err
	}
	ti.seen[ev.DeliveryID]++
	if ti.seen[ev.DeliveryID] == 1 {
		// First delivery — record it as a genuine ingest.
		ti.ingested = append(ti.ingested, ev)
	}
	// Duplicate — silently drop (mirrors ON CONFLICT DO NOTHING + RowsAffected==0 path).
	return nil
}

// ingestCount returns the number of first-time (non-replay) ingests recorded.
func (ti *trackingIngestor) ingestCount() int {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return len(ti.ingested)
}

// callsFor returns the total number of times Ingest was called for deliveryID
// (including replays).
func (ti *trackingIngestor) callsFor(deliveryID string) int {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return ti.seen[deliveryID]
}

// ingested events snapshot (copy).
func (ti *trackingIngestor) ingestedEvents() []events.GitHubWebhookReceived {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	out := make([]events.GitHubWebhookReceived, len(ti.ingested))
	copy(out, ti.ingested)
	return out
}

// ---------------------------------------------------------------------------
// helpers for replay-protection tests
// ---------------------------------------------------------------------------

// webhookBody builds a minimal but valid JSON payload for the given event type.
func webhookBody(event, action, repoFullName string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"repository": map[string]any{
			"full_name": repoFullName,
		},
	})
	return b
}

// deliverWebhook sends one webhook POST to app and returns the HTTP status code.
// It signs the body with secret and sets all required GitHub headers.
func deliverWebhook(app *fiber.App, secret, deliveryID, event string, body []byte) int {
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      event,
		"X-GitHub-Delivery":   deliveryID,
		"X-Hub-Signature-256": sign(secret, body),
	})
	resp.Body.Close()
	return resp.StatusCode
}

// ---------------------------------------------------------------------------
// Replay-protection: inline ingestor path
// ---------------------------------------------------------------------------

// TestReceive_ReplayRejected_InlinePath verifies the core security property:
// sending the same delivery (identical body, valid signature, same delivery
// ID) twice through the HTTP handler results in exactly one logical ingest —
// the second delivery is silently absorbed (HTTP 200 is returned so GitHub
// does not re-queue, but the ingestor's dedup gate suppresses the side-
// effects).
//
// This closes the loop between the handler and the ingestor's
// webhook_delivery_dedup protection: if the handler did not invoke the
// ingestor at all, or called it with the wrong delivery ID, the duplicate
// would either fail or be processed twice.
func TestReceive_ReplayRejected_InlinePath(t *testing.T) {
	const secret = "replay-secret"
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	body := webhookBody("issues", "opened", "owner/repo")

	// First delivery — must be accepted and ingested.
	if code := deliverWebhook(app, secret, "del-replay-1", "issues", body); code != fiber.StatusOK {
		t.Fatalf("first delivery: want 200, got %d", code)
	}

	// Replay — same delivery ID, same body, same sig.  HTTP returns 200 (so
	// GitHub does not retry) but no new logical ingest should occur.
	if code := deliverWebhook(app, secret, "del-replay-1", "issues", body); code != fiber.StatusOK {
		t.Fatalf("replay delivery: want 200, got %d", code)
	}

	// Ingestor must have been called twice (handler invokes it both times)…
	if got := ti.callsFor("del-replay-1"); got != 2 {
		t.Fatalf("want Ingest called 2×, got %d", got)
	}
	// …but only one genuine ingest recorded (dedup gate fires on second call).
	if got := ti.ingestCount(); got != 1 {
		t.Fatalf("want 1 logical ingest, got %d (replay was not deduplicated)", got)
	}
}

// TestReceive_ReplayRejected_AllEventTypes exercises every event type the
// handler routes through the ingestor.  For each type a first delivery must
// be accepted and exactly one logical ingest produced; a byte-identical replay
// must return 200 but produce no additional ingest.
func TestReceive_ReplayRejected_AllEventTypes(t *testing.T) {
	const secret = "alltype-secret"

	eventTypes := []struct {
		event  string
		action string
	}{
		{"issues", "opened"},
		{"pull_request", "opened"},
		{"pull_request_review", "submitted"},
		{"installation", "created"},
		{"installation_repositories", "added"},
		{"ping", ""},
		{"push", ""},
	}

	for _, tc := range eventTypes {
		tc := tc // capture
		t.Run(tc.event, func(t *testing.T) {
			ti := newTrackingIngestor()
			h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
			h.ing = ti
			app := newTestApp(h)

			body := webhookBody(tc.event, tc.action, "owner/repo")
			deliveryID := "replay-type-" + tc.event

			// First delivery.
			if code := deliverWebhook(app, secret, deliveryID, tc.event, body); code != fiber.StatusOK {
				t.Fatalf("first delivery: want 200, got %d", code)
			}
			if got := ti.ingestCount(); got != 1 {
				t.Fatalf("after first delivery: want 1 ingest, got %d", got)
			}

			// Replay.
			if code := deliverWebhook(app, secret, deliveryID, tc.event, body); code != fiber.StatusOK {
				t.Fatalf("replay: want 200, got %d", code)
			}
			if got := ti.ingestCount(); got != 1 {
				t.Fatalf("after replay: want still 1 ingest, got %d", got)
			}
		})
	}
}

// TestReceive_ReplayRejected_SameBodyDifferentDeliveryID confirms that a
// delivery with the same payload but a different delivery ID is treated as a
// new event and processed — dedup is keyed on delivery ID, not body content.
func TestReceive_ReplayRejected_SameBodyDifferentDeliveryID(t *testing.T) {
	const secret = "diffid-secret"
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	body := webhookBody("issues", "opened", "owner/repo")

	for i, id := range []string{"del-diffid-A", "del-diffid-B"} {
		if code := deliverWebhook(app, secret, id, "issues", body); code != fiber.StatusOK {
			t.Fatalf("delivery %d: want 200, got %d", i+1, code)
		}
	}

	// Both should have been ingested — they have distinct delivery IDs.
	if got := ti.ingestCount(); got != 2 {
		t.Fatalf("want 2 ingests (different IDs), got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Replay-protection: NATS path
// ---------------------------------------------------------------------------

// TestReceive_Replay_NATSPath_BothPublished verifies the NATS behaviour: the
// handler publishes every delivery that passes signature verification to the
// bus without deduplicating them.  Replay protection on the NATS path lives
// in the worker consumer, not in the handler.  This test documents and
// asserts that contract so that if someone accidentally adds dedup at the
// handler level (breaking the "return 200 immediately" contract) the test
// catches it.
func TestReceive_Replay_NATSPath_BothPublished(t *testing.T) {
	const secret = "nats-replay-secret"
	bus := &mockBus{}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, bus)
	app := newTestApp(h)

	body := webhookBody("issues", "opened", "owner/repo")

	// Send the same delivery twice.
	for i := 0; i < 2; i++ {
		if code := deliverWebhook(app, secret, "del-nats-replay", "issues", body); code != fiber.StatusOK {
			t.Fatalf("delivery %d: want 200, got %d", i+1, code)
		}
	}

	// The handler must have published to NATS twice — dedup is downstream.
	if got := len(bus.published()); got != 2 {
		t.Fatalf("NATS path: want 2 publishes for 2 deliveries, got %d", got)
	}
	// Both published payloads must carry the same delivery ID so the
	// downstream worker can dedup correctly.
	for _, msg := range bus.published() {
		var ev events.GitHubWebhookReceived
		if err := json.Unmarshal(msg.data, &ev); err != nil {
			t.Fatalf("unmarshal NATS msg: %v", err)
		}
		if ev.DeliveryID != "del-nats-replay" {
			t.Errorf("NATS msg delivery_id: want del-nats-replay, got %q", ev.DeliveryID)
		}
	}
}

// ---------------------------------------------------------------------------
// Bad-signature short-circuits before ingestor (fail-closed)
// ---------------------------------------------------------------------------

// TestReceive_BadSig_IngestorNeverCalled is the key "fail-closed" test:
// a delivery with an invalid signature must never reach the ingestor,
// regardless of event type.  If the ingestor were called on an invalid
// signature, an attacker could forge events by replaying old deliveries with
// modified payloads.
func TestReceive_BadSig_IngestorNeverCalled(t *testing.T) {
	const secret = "failclosed-secret"

	for _, event := range []string{
		"issues", "pull_request", "pull_request_review",
		"installation", "installation_repositories", "ping", "push",
	} {
		event := event
		t.Run(event, func(t *testing.T) {
			ti := newTrackingIngestor()
			h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
			h.ing = ti
			app := newTestApp(h)

			body := webhookBody(event, "opened", "owner/repo")

			resp := doRequest(app, body, map[string]string{
				"Content-Type":        "application/json",
				"X-GitHub-Event":      event,
				"X-GitHub-Delivery":   "bad-sig-" + event,
				"X-Hub-Signature-256": "sha256=badc0ffee", // wrong
			})
			resp.Body.Close()

			if resp.StatusCode != fiber.StatusUnauthorized {
				t.Fatalf("want 401 on bad sig, got %d", resp.StatusCode)
			}
			// Critical: ingestor must not have been touched.
			if got := ti.callsFor("bad-sig-" + event); got != 0 {
				t.Fatalf("ingestor called %d times despite bad signature — fail-open bug", got)
			}
		})
	}
}

// TestReceive_BadSig_NATSPathNeverPublished confirms that with a NATS bus
// configured, a bad signature also prevents publication — the bus is not
// a side-channel that bypasses the signature gate.
func TestReceive_BadSig_NATSPathNeverPublished(t *testing.T) {
	const secret = "nats-failclosed-secret"
	bus := &mockBus{}
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, bus)
	app := newTestApp(h)

	body := webhookBody("issues", "opened", "owner/repo")
	resp := doRequest(app, body, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "bad-sig-nats",
		"X-Hub-Signature-256": "sha256=0000000000",
	})
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if got := len(bus.published()); got != 0 {
		t.Fatalf("want 0 NATS publishes on bad sig, got %d", got)
	}
}

// TestReceive_TamperedBody_InvalidatesSignature verifies that altering even
// one byte of the body after signing causes rejection.  This prevents an
// attacker from replaying a legitimately-signed delivery with a modified
// payload.
func TestReceive_TamperedBody_InvalidatesSignature(t *testing.T) {
	const secret = "tamper-secret"
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	original := webhookBody("issues", "opened", "owner/repo")
	// Sign the original, then deliver a tampered body with the original signature.
	sig := sign(secret, original)
	tampered := append([]byte(nil), original...) // copy
	tampered[len(tampered)-1] ^= 0x01            // flip one bit

	resp := doRequest(app, tampered, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "tampered-delivery",
		"X-Hub-Signature-256": sig,
	})
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("want 401 for tampered body, got %d", resp.StatusCode)
	}
	if got := ti.callsFor("tampered-delivery"); got != 0 {
		t.Fatalf("ingestor called %d times for tampered delivery — must be 0", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrent deliveries — shared dedup state
// ---------------------------------------------------------------------------

// TestReceive_ConcurrentDeliveries_DifferentIDs confirms that N goroutines
// each sending a distinct, valid delivery all result in N logical ingests —
// the dedup mechanism does not incorrectly coalesce different delivery IDs
// when they arrive simultaneously.
func TestReceive_ConcurrentDeliveries_DifferentIDs(t *testing.T) {
	const (
		secret = "concurrent-secret"
		n      = 20
	)
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-del-%d", i)
			body := webhookBody("issues", "opened", fmt.Sprintf("owner/repo-%d", i))
			if code := deliverWebhook(app, secret, id, "issues", body); code != fiber.StatusOK {
				t.Errorf("goroutine %d: want 200, got %d", i, code)
			}
		}(i)
	}
	wg.Wait()

	if got := ti.ingestCount(); got != n {
		t.Fatalf("want %d ingests (one per distinct ID), got %d", n, got)
	}
}

// TestReceive_ConcurrentDeliveries_SameID simulates a thundering-herd replay:
// N goroutines all attempt to deliver the same delivery ID concurrently.
// Exactly one must result in a logical ingest; the rest must be absorbed by
// the dedup gate.  HTTP status must be 200 for all (so GitHub does not
// re-queue any of them).
func TestReceive_ConcurrentDeliveries_SameID(t *testing.T) {
	const (
		secret = "concurrent-same-secret"
		n      = 20
	)
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	body := webhookBody("issues", "opened", "owner/repo")

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if code := deliverWebhook(app, secret, "same-id-concurrent", "issues", body); code != fiber.StatusOK {
				t.Errorf("goroutine %d: want 200, got %d", i, code)
			}
		}(i)
	}
	wg.Wait()

	// All n goroutines called Ingest…
	if got := ti.callsFor("same-id-concurrent"); got != n {
		t.Fatalf("want Ingest called %d times, got %d", n, got)
	}
	// …but dedup must have suppressed all but the first.
	if got := ti.ingestCount(); got != 1 {
		t.Fatalf("want exactly 1 logical ingest, got %d (dedup failed under concurrency)", got)
	}
}

// ---------------------------------------------------------------------------
// First-time delivery acceptance — all handled event types
// ---------------------------------------------------------------------------

// TestReceive_FirstTimeDelivery_AllEventTypes verifies acceptance criterion 3:
// legitimate first-time deliveries for every currently-handled event type
// return 200 and are forwarded to the ingestor exactly once.
func TestReceive_FirstTimeDelivery_AllEventTypes(t *testing.T) {
	const secret = "firsttime-secret"

	cases := []struct {
		event  string
		action string
	}{
		{"issues", "opened"},
		{"issues", "closed"},
		{"pull_request", "opened"},
		{"pull_request", "closed"},
		{"pull_request_review", "submitted"},
		{"installation", "created"},
		{"installation", "deleted"},
		{"installation_repositories", "added"},
		{"installation_repositories", "removed"},
		{"ping", ""},
		{"push", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.event+"/"+tc.action, func(t *testing.T) {
			ti := newTrackingIngestor()
			h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
			h.ing = ti
			app := newTestApp(h)

			body := webhookBody(tc.event, tc.action, "owner/repo")
			id := "firsttime-" + tc.event + "-" + tc.action

			if code := deliverWebhook(app, secret, id, tc.event, body); code != fiber.StatusOK {
				t.Fatalf("want 200, got %d", code)
			}
			if got := ti.ingestCount(); got != 1 {
				t.Fatalf("want 1 ingest, got %d", got)
			}
			ev := ti.ingestedEvents()[0]
			if ev.DeliveryID != id {
				t.Errorf("delivery_id: want %q, got %q", id, ev.DeliveryID)
			}
			if ev.Event != tc.event {
				t.Errorf("event: want %q, got %q", tc.event, ev.Event)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Replay with valid-but-reused signature
// ---------------------------------------------------------------------------

// TestReceive_ValidSigReusedDeliveryID confirms that a delivery that is
// cryptographically valid (correct signature, correct body) but carries a
// previously-seen delivery ID is still deduplicated.  This is the realistic
// attack scenario: an attacker captures a legitimate delivery and replays it
// verbatim.
func TestReceive_ValidSigReusedDeliveryID(t *testing.T) {
	const secret = "reused-id-secret"
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	body := webhookBody("pull_request", "opened", "owner/repo")

	// Legitimate first delivery.
	if code := deliverWebhook(app, secret, "reused-delivery-id", "pull_request", body); code != fiber.StatusOK {
		t.Fatalf("first: want 200, got %d", code)
	}

	// Replay: same delivery ID, same body, same valid signature.
	if code := deliverWebhook(app, secret, "reused-delivery-id", "pull_request", body); code != fiber.StatusOK {
		t.Fatalf("replay: want 200, got %d", code)
	}

	// Handler called ingestor twice but dedup kept only the first.
	if got := ti.callsFor("reused-delivery-id"); got != 2 {
		t.Fatalf("want Ingest called 2×, got %d", got)
	}
	if got := ti.ingestCount(); got != 1 {
		t.Fatalf("want 1 logical ingest, got %d", got)
	}
}

// TestReceive_TwoDifferentEventTypes_SameDeliveryID verifies the edge case
// where the same delivery ID is reused across two different event types
// (which GitHub would never do, but an attacker might try).  The second
// delivery must be treated as a replay regardless of the event type change.
func TestReceive_TwoDifferentEventTypes_SameDeliveryID(t *testing.T) {
	const secret = "cross-event-secret"
	ti := newTrackingIngestor()
	h := NewGitHubWebhooksHandler(config.Config{GitHubWebhookSecret: secret}, nil, nil)
	h.ing = ti
	app := newTestApp(h)

	issuesBody := webhookBody("issues", "opened", "owner/repo")
	prBody := webhookBody("pull_request", "opened", "owner/repo")

	// First: issues event.
	if code := deliverWebhook(app, secret, "cross-event-id", "issues", issuesBody); code != fiber.StatusOK {
		t.Fatalf("issues delivery: want 200, got %d", code)
	}
	// Second: pull_request event, different body, same delivery ID.
	prSig := sign(secret, prBody)
	resp := doRequest(app, prBody, map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "cross-event-id",
		"X-Hub-Signature-256": prSig,
	})
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("pr delivery: want 200, got %d", resp.StatusCode)
	}

	// Dedup is by delivery ID: only the first ingest is genuine.
	if got := ti.ingestCount(); got != 1 {
		t.Fatalf("want 1 ingest, got %d (cross-event replay not deduplicated)", got)
	}
	// The genuine ingest must be the issues event (the first one).
	if ev := ti.ingestedEvents()[0]; ev.Event != "issues" {
		t.Errorf("first ingested event: want issues, got %q", ev.Event)
	}
}
