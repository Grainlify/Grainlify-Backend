package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// An unknown kind is refused whether or not the person has an account. The
// two checks used to run the other way round, so an unrecognised kind for
// somebody with no account came back 200 - the agent marked it delivered and
// the event was gone. This test passed locally only because another test had
// left a user with that GitHub id behind.
func TestBountyEvents_RefusesAKindItDoesNotKnow(t *testing.T) {
	app := eventsApp(t, eventsSecret)
	for _, id := range []int64{1, 999999999} {
		body := fmt.Sprintf(`{"kind":"something_new","githubUserId":%d,"payload":{}}`, id)
		if code := post(t, app, body, signed(body)); code != 400 {
			t.Fatalf("githubUserId %d: status = %d, want 400", id, code)
		}
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

// One test per kind, asserting what the person actually ends up reading.
//
// The tests above check the channel - signature, unknown kinds, unknown
// recipients - and none of them check the message. A kind could map to an
// empty body, or to the wrong notification type (and so be governed by the
// wrong preference switch, and land under the wrong heading), and every test
// here would still pass. These read the row back out.
func TestBountyEvents_EachKindStoresAReadableNotification(t *testing.T) {
	base := map[string]any{
		"repo": "Grainlify/grainlify-agent-sandbox", "issue_number": float64(5),
		"amount_minor": "25000000", "currency": "USDC",
	}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		kind     string
		wantType notifications.Type
		payload  map[string]any
		// Fragments the body must contain. Not the whole string: wording is
		// allowed to improve, but the facts somebody needs are not optional.
		wantIn   []string
		wantLink notifications.Link
	}{
		{
			kind: "bounty_draw_won", wantType: notifications.TypeBountyDrawWon,
			payload: with(map[string]any{"staleAt": "2026-10-09T04:21:42Z"}),
			// The amount, where, what to do, by when, and what happens if not.
			wantIn:   []string{"25 USDC", "Grainlify/grainlify-agent-sandbox #5", "Closes #5", "2026-10-09T04:21:42Z", "abandon"},
			wantLink: notifications.BountiesLink(),
		},
		{
			kind: "bounty_assignment_expiring", wantType: notifications.TypeBountyAssignmentExpiring,
			payload: with(map[string]any{"hoursLeft": float64(24)}),
			// Says plainly that letting it lapse counts as an abandon.
			wantIn:   []string{"25 USDC", "Grainlify/grainlify-agent-sandbox #5", "abandon"},
			wantLink: notifications.BountyRulesLink(),
		},
		{
			kind: "bounty_paid", wantType: notifications.TypeBountyPaid,
			payload:  with(map[string]any{"txUrl": "https://solscan.io/tx/abc"}),
			wantIn:   []string{"25 USDC", "https://solscan.io/tx/abc"},
			wantLink: notifications.BountiesLink(),
		},
		{
			kind: "bounty_application_received", wantType: notifications.TypeBountyApplicationReceived,
			payload:  with(map[string]any{"closesAt": "2026-10-01T12:00:00Z"}),
			wantIn:   []string{"Grainlify/grainlify-agent-sandbox #5", "2026-10-01T12:00:00Z", "draw"},
			wantLink: notifications.BountiesLink(),
		},
		{
			kind: "bounty_draw_lost", wantType: notifications.TypeBountyDrawLost,
			payload: base,
			// Says the loss costs them nothing, because silence after a draw
			// reads as being penalised for applying.
			wantIn:   []string{"Grainlify/grainlify-agent-sandbox #5", "unaffected"},
			wantLink: notifications.BountiesLink(),
		},
		{
			kind: "bounty_review_posted", wantType: notifications.TypeBountyReviewPosted,
			payload: base,
			// Says the review does not decide anything, so nobody reads an
			// advisory review as a rejection.
			wantIn:   []string{"Grainlify/grainlify-agent-sandbox #5", "maintainer"},
			wantLink: notifications.BountiesLink(),
		},
	}

	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			d := dbtest.DB(t)
			uid := newUser(t, d)
			ghID := linkGitHub(t, d, uid, "applicant")
			h := NewBountyEventsHandler(d, notifications.New(d, nil, "https://grainlify.com"), eventsSecret)
			app := fiber.New()
			app.Post("/internal/bounty-events", h.Receive)

			raw, _ := json.Marshal(map[string]any{"kind": c.kind, "githubUserId": ghID, "payload": c.payload})
			if code := post(t, app, string(raw), signed(string(raw))); code != 200 {
				t.Fatalf("status = %d, want 200", code)
			}

			var gotType, title, body, link string
			if err := d.Pool.QueryRow(context.Background(), `
SELECT type, title, body, link_path FROM notifications WHERE user_id = $1
`, uid).Scan(&gotType, &title, &body, &link); err != nil {
				t.Fatalf("no notification stored for %s: %v", c.kind, err)
			}

			if gotType != string(c.wantType) {
				t.Errorf("type = %q, want %q - a wrong type means the wrong preference switch governs it", gotType, c.wantType)
			}
			if strings.TrimSpace(title) == "" {
				t.Error("title is empty")
			}
			for _, fragment := range c.wantIn {
				if !strings.Contains(body, fragment) {
					t.Errorf("body is missing %q:\n%s", fragment, body)
				}
			}
			if link != c.wantLink.String() {
				t.Errorf("link_path = %q, want %q", link, c.wantLink)
			}
		})
	}
}

// Every bounty type must be one the preferences screen can switch off. The
// notifications package has its own test for this over all types; this one
// names the six that arrive through this handler, so a kind added here
// without a preference fails in the file that added it.
func TestBountyEvents_EveryKindIsSwitchable(t *testing.T) {
	for _, ty := range []notifications.Type{
		notifications.TypeBountyDrawWon,
		notifications.TypeBountyAssignmentExpiring,
		notifications.TypeBountyPaid,
		notifications.TypeBountyApplicationReceived,
		notifications.TypeBountyDrawLost,
		notifications.TypeBountyReviewPosted,
	} {
		if !ty.Valid() {
			t.Errorf("%q is not in notifications.AllTypes, so nobody can turn it off", ty)
		}
	}
}
