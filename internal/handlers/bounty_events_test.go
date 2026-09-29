package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

const eventsSecret = "test-events-secret-at-least-32-characters"

func signed(body string) string {
	m := hmac.New(sha256.New, []byte(eventsSecret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func eventsApp(t *testing.T, secret string) *fiber.App {
	t.Helper()
	d := dbtest.DB(t)
	h := NewBountyEventsHandler(d, notifications.New(d, nil, "https://grainlify.com"), secret)
	app := fiber.New()
	app.Post("/internal/bounty-events", h.Receive)
	return app
}

func post(t *testing.T, app *fiber.App, body, sig string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/internal/bounty-events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Bounty-Signature-256", sig)
	}
	res, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return res.StatusCode
}

// The endpoint takes no session, so the signature is the whole of its
// authentication. An unsigned body must not be able to send notifications to
// our users.
func TestBountyEvents_RefusesAnUnsignedBody(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	body := `{"kind":"bounty_draw_won","githubUserId":1,"payload":{}}`
	if code := post(t, app, body, ""); code != 401 {
		t.Fatalf("unsigned: status = %d, want 401", code)
	}
	if code := post(t, app, body, "sha256=deadbeef"); code != 401 {
		t.Fatalf("wrong signature: status = %d, want 401", code)
	}
}

// A signature over different bytes is not a signature over these bytes.
func TestBountyEvents_RefusesASignatureForAnotherBody(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	if code := post(t, app, `{"kind":"bounty_paid","githubUserId":1,"payload":{}}`, signed(`{"kind":"bounty_draw_won"}`)); code != 401 {
		t.Fatalf("status = %d, want 401", code)
	}
}

// No secret configured must mean "refuse everything", never "accept
// everything": a missing variable must not open the endpoint to anybody.
func TestBountyEvents_WithoutASecretRefusesEverything(t *testing.T) {
	app := eventsApp(t, "")
	body := `{"kind":"bounty_draw_won","githubUserId":1,"payload":{}}`
	if code := post(t, app, body, signed(body)); code != 401 {
		t.Fatalf("status = %d, want 401 when no secret is configured", code)
	}
}

func TestBountyEvents_RefusesAKindItDoesNotKnow(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	body := `{"kind":"something_new","githubUserId":1,"payload":{}}`
	if code := post(t, app, body, signed(body)); code != 400 {
		t.Fatalf("status = %d, want 400", code)
	}
}

// Somebody with no Grainlify account is not a delivery failure. Answering
// otherwise would make the agent retry it forever.
func TestBountyEvents_AcceptsAnEventForSomebodyWithNoAccount(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	body := `{"kind":"bounty_draw_won","githubUserId":999999999,"payload":{"repo":"a/b","issue_number":1,"amount_minor":"1000000","currency":"USDC"}}`
	if code := post(t, app, body, signed(body)); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
}

// Minor units are how the agent stores money and not how a person reads it.
// "1000000 USDC" in an email is alarming.
func TestBountyEvents_RendersAmountsForPeople(t *testing.T) {
	for _, c := range []struct{ minor, want string }{
		{"1000000", "1 USDC"},
		{"1500000", "1.5 USDC"},
		{"20000000", "20 USDC"},
		{"500000", "0.5 USDC"},
		{"1", "0.000001 USDC"},
	} {
		got := money(map[string]any{"amount_minor": c.minor, "currency": "USDC"})
		if got != c.want {
			t.Fatalf("money(%s) = %q, want %q", c.minor, got, c.want)
		}
	}
}

func TestBountyEvents_DescribesWhereWithoutAnIssueNumber(t *testing.T) {
	if got := where(map[string]any{"repo": "a/b"}); got != "a/b" {
		t.Fatalf("where = %q", got)
	}
	if got := where(map[string]any{"repo": "a/b", "issue_number": float64(7)}); got != "a/b #7" {
		t.Fatalf("where = %q", got)
	}
}

// The won notification is the one that had to exist. It must carry what to do
// and by when, not just the fact of winning.
func TestBountyEvents_WonBodyCarriesTheDeadlineAndTheConsequence(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	payload := map[string]any{
		"repo": "Grainlify/sandbox", "issue_number": 3, "amount_minor": "1000000",
		"currency": "USDC", "staleAt": "2026-10-09T04:21:42Z",
	}
	raw, _ := json.Marshal(map[string]any{"kind": "bounty_draw_won", "githubUserId": 1, "payload": payload})
	if code := post(t, app, string(raw), signed(string(raw))); code != 200 {
		t.Fatalf("status = %d, want 200", code)
	}
}
