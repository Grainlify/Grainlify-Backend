package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// The property this endpoint exists to hold: a support request is written down
// before anybody tries to deliver it.
//
// It previously relayed straight to a Discord webhook and stored nothing - its
// own comment named the Discord channel as the system of record - so a failed
// webhook returned 502 and the report was gone along with whatever the person
// had typed. Every test here is about that, or about the identity the endpoint
// used to take on trust from the request body.

const supportTestSecret = "support-test-secret"

// supportApp also clears the last hour of support rows.
//
// The endpoint now enforces a GLOBAL cap on anonymous submissions per hour,
// counted from the table rather than held in memory - so it is stateful, and
// dbtest.DB hands out a shared database. Without this, support tests
// accumulate anonymous rows across a package run, cross the cap partway
// through, and every test after that point gets a 429 from a limit none of
// them are about. That is not a test artefact: it is the same statefulness
// behaving correctly, and seeing it here rather than in production is the
// cheap version.
func supportApp(d *db.DB) *fiber.App {
	if d != nil && d.Pool != nil {
		_, _ = d.Pool.Exec(context.Background(), `
DELETE FROM support_requests WHERE created_at > now() - interval '1 hour'
`)
	}
	h := handlers.NewSupportRequestsHandler(
		// No Discord webhook: the sink is unconfigured and skipped, which is
		// the state that used to 503 the whole endpoint.
		config.Config{JWTSecret: supportTestSecret},
		d,
	)
	app := fiber.New()
	app.Post("/support-requests", h.Create())
	return app
}

func postSupport(t *testing.T, app *fiber.App, body string, bearer string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/support-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "test-agent/1.0")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, 15000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSupport_PersistsBeforeAnyDelivery(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	code, body := postSupport(t, app, `{"category":"bug","message":"the leaderboard shows the wrong rank","page_url":"https://grainlify.com/dashboard"}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("returned %d: %v", code, body)
	}
	idStr, _ := body["support_id"].(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		t.Fatalf("no usable support_id in response: %v", body)
	}

	var message, category string
	var pageURL, userAgent *string
	var discordAt, telegramAt *string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT message, category, page_url, user_agent,
       discord_delivered_at::text, telegram_delivered_at::text
FROM support_requests WHERE id = $1`, id,
	).Scan(&message, &category, &pageURL, &userAgent, &discordAt, &telegramAt); err != nil {
		t.Fatalf("row was not written: %v", err)
	}

	if message != "the leaderboard shows the wrong rank" {
		t.Errorf("message = %q", message)
	}
	if category != "bug" {
		t.Errorf("category = %q, want bug", category)
	}
	if pageURL == nil || *pageURL != "https://grainlify.com/dashboard" {
		t.Errorf("page_url = %v", pageURL)
	}
	if userAgent == nil || *userAgent != "test-agent/1.0" {
		t.Errorf("user_agent = %v", userAgent)
	}
	// No sink is configured here, so nothing was delivered - and the report is
	// still saved and the caller still got a 200. That is the whole point: the
	// old endpoint returned 503 when Discord was unconfigured and 502 when it
	// was down, losing the report either way.
	if discordAt != nil || telegramAt != nil {
		t.Errorf("nothing should be marked delivered with no sink configured: discord=%v telegram=%v", discordAt, telegramAt)
	}
}

func TestSupport_SucceedsWithNoSinkConfigured(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	code, body := postSupport(t, app, `{"message":"anything"}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("an unconfigured sink must not fail the request; got %d: %v", code, body)
	}
	if delivered, ok := body["delivered"].([]any); !ok || len(delivered) != 0 {
		t.Errorf("delivered = %v, want an empty list", body["delivered"])
	}
}

// Identity comes from the token. The old endpoint took a `reporter_login`
// string from the request body and relayed it as the reporter, so anyone could
// file a report as anyone - harmless-looking while the sink was a private
// Discord, an impersonation vector the moment a sink is a public group.
func TestSupport_IdentityComesFromTheTokenNotTheBody(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	realUser := leaderboardSuiteUser(t, d.Pool)
	token, err := auth.IssueJWT(supportTestSecret, realUser, "contributor", "", "", 3600_000_000_000)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}

	// The body claims to be somebody else. It must be ignored entirely.
	code, body := postSupport(t, app,
		`{"message":"filed while signed in","reporter_login":"someone-else","user_id":"00000000-0000-0000-0000-000000000001"}`,
		token)
	if code != fiber.StatusOK {
		t.Fatalf("returned %d: %v", code, body)
	}
	id := uuid.MustParse(body["support_id"].(string))

	var storedUser *uuid.UUID
	if err := d.Pool.QueryRow(t.Context(),
		`SELECT user_id FROM support_requests WHERE id = $1`, id).Scan(&storedUser); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if storedUser == nil || *storedUser != realUser {
		t.Fatalf("user_id = %v, want %v resolved from the token", storedUser, realUser)
	}
}

func TestSupport_AnonymousIsAllowedAndCarriesNoIdentity(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	for _, bearer := range []string{"", "not-a-real-jwt", "Bearer-ish-nonsense"} {
		code, body := postSupport(t, app, `{"message":"I cannot sign in at all"}`, bearer)
		if code != fiber.StatusOK {
			t.Fatalf("bearer %q: returned %d - somebody who cannot sign in is the person most likely to need support", bearer, code)
		}
		id := uuid.MustParse(body["support_id"].(string))
		var storedUser *uuid.UUID
		_ = d.Pool.QueryRow(t.Context(), `SELECT user_id FROM support_requests WHERE id = $1`, id).Scan(&storedUser)
		if storedUser != nil {
			t.Errorf("bearer %q: user_id = %v, want NULL", bearer, *storedUser)
		}
	}
}

// The IP is recorded for abuse investigation and must never reach a sink. It
// was a field on the Discord embed; a public Telegram topic must not see it.
func TestSupport_RecordsIPOnTheRowOnly(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	code, body := postSupport(t, app, `{"message":"checking ip handling"}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("returned %d", code)
	}
	id := uuid.MustParse(body["support_id"].(string))

	var ip *string
	_ = d.Pool.QueryRow(t.Context(), `SELECT reporter_ip FROM support_requests WHERE id = $1`, id).Scan(&ip)
	if ip == nil || *ip == "" {
		t.Error("reporter_ip was not recorded; it is needed for abuse investigation")
	}
	// And it must not be echoed back to the caller either.
	for k, v := range body {
		if s, ok := v.(string); ok && ip != nil && s == *ip {
			t.Errorf("response field %q leaked the reporter IP", k)
		}
	}
}

func TestSupport_ValidatesCategoryAndMessage(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	cases := []struct {
		body string
		want string
	}{
		{`{"message":""}`, "message_required"},
		{`{}`, "message_required"},
		{`{"message":"hi","category":"nonsense"}`, "invalid_category"},
		{`{"message":"hi","category":"BUGS"}`, "invalid_category"},
		{`{"message":"` + strings.Repeat("x", 2001) + `"}`, "message_too_long"},
	}
	for _, tc := range cases {
		code, out := postSupport(t, app, tc.body, "")
		if code != fiber.StatusBadRequest {
			t.Errorf("body %.40s returned %d, want 400", tc.body, code)
			continue
		}
		if out["error"] != tc.want {
			t.Errorf("body %.40s: error = %v, want %v", tc.body, out["error"], tc.want)
		}
	}

	// Every widget category must be accepted, or a picker option 500s in
	// production against the CHECK constraint.
	for _, cat := range []string{"bug", "kyc", "idea", "help", "other"} {
		code, out := postSupport(t, app, `{"category":"`+cat+`","message":"category coverage"}`, "")
		if code != fiber.StatusOK {
			t.Errorf("category %q returned %d: %v", cat, code, out)
		}
	}
}

// The legacy field name keeps working, so a cached frontend bundle does not
// start failing the moment this deploys.
func TestSupport_AcceptsTheLegacyDescriptionField(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	code, body := postSupport(t, app, `{"description":"sent by an old bundle"}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("returned %d: %v", code, body)
	}
	id := uuid.MustParse(body["support_id"].(string))
	var message, category string
	_ = d.Pool.QueryRow(t.Context(), `SELECT message, category FROM support_requests WHERE id = $1`, id).Scan(&message, &category)
	if message != "sent by an old bundle" {
		t.Errorf("message = %q", message)
	}
	if category != "bug" {
		t.Errorf("category = %q, want the bug default", category)
	}
}

func TestSupport_StoresTheScreenshot(t *testing.T) {
	d := testDB(t)
	app := supportApp(d)

	// 1x1 transparent PNG.
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
	code, body := postSupport(t, app, `{"message":"with screenshot","screenshot":"`+png+`"}`, "")
	if code != fiber.StatusOK {
		t.Fatalf("returned %d: %v", code, body)
	}
	id := uuid.MustParse(body["support_id"].(string))

	var stored *string
	_ = d.Pool.QueryRow(t.Context(), `SELECT screenshot_url FROM support_requests WHERE id = $1`, id).Scan(&stored)
	if stored == nil || *stored != png {
		t.Error("screenshot was not stored; a bug report without it is often not actionable")
	}

	// A non-image is refused before anything is written.
	code, out := postSupport(t, app, `{"message":"bad shot","screenshot":"data:application/pdf;base64,AAAA"}`, "")
	if code != fiber.StatusBadRequest || out["error"] != "screenshot_must_be_an_image" {
		t.Errorf("non-image screenshot: got %d %v", code, out["error"])
	}
}
