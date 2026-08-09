package handlers_test

// Scope note: covers handlers.BugReportsHandler (internal/handlers/bug_reports.go).
// Unlike most other *_test.go files in this package, none of these tests
// call testDB(t) - Create() never touches Postgres, it only validates the
// request and relays to a Discord webhook URL taken straight from
// config.Config, which (unlike internal/github's unexported API-base-URL
// vars) is trivially redirectable to an httptest.NewServer per test.
//
// The route is registered here WITHOUT the limiter.New(...) middleware
// api.go wraps it with in production: several subtests below post to the
// same *fiber.App from the same fake IP, which would trip the 5-req/min cap
// well before the suite finishes. Rate limiting itself is well-tested Fiber
// built-in middleware, not custom logic worth re-verifying here.

import (
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// bugReportsSuiteTinyPNG is a valid, minimal 1x1 transparent PNG, base64-encoded.
const bugReportsSuiteTinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func bugReportsSuiteApp(cfg config.Config) *fiber.App {
	h := handlers.NewBugReportsHandler(cfg)
	// BodyLimit must mirror internal/api/api.go's production config (default
	// Fiber is 4MB) - otherwise an oversized-screenshot test case gets
	// rejected by Fiber itself before ever reaching Create()'s own
	// screenshot_too_large check, which is the exact latent gap this test
	// suite is here to catch.
	app := fiber.New(fiber.Config{BodyLimit: 8 * 1024 * 1024})
	app.Post("/bug-reports", h.Create())
	return app
}

func bugReportsSuiteDo(t *testing.T, app *fiber.App, payload any) (int, map[string]any) {
	t.Helper()
	status, body := projectsFxDoJSON(t, app, "POST", "/bug-reports", "", payload)
	var resp map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("bugReportsSuiteDo: decode response: %v, body=%s", err, body)
		}
	}
	return status, resp
}

func TestBugReportsHandler_Create_NotConfigured(t *testing.T) {
	app := bugReportsSuiteApp(config.Config{}) // DiscordBugReportWebhookURL deliberately unset

	status, resp := bugReportsSuiteDo(t, app, map[string]any{"description": "the sign-in button does nothing"})
	if status != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, resp=%v", status, resp)
	}
	if resp["error"] != "bug_reports_not_configured" {
		t.Errorf("error = %v, want bug_reports_not_configured", resp["error"])
	}
}

func TestBugReportsHandler_Create_Validation(t *testing.T) {
	// A configured-but-unreachable webhook URL: every case below must fail
	// validation before the handler ever attempts to dial out, so this must
	// never actually be hit.
	app := bugReportsSuiteApp(config.Config{DiscordBugReportWebhookURL: "http://127.0.0.1:1/unreachable"})

	cases := []struct {
		name      string
		payload   map[string]any
		wantError string
	}{
		{"empty description", map[string]any{"description": ""}, "description_required"},
		{"whitespace-only description", map[string]any{"description": "   \n\t  "}, "description_required"},
		{"description too long", map[string]any{"description": strings.Repeat("a", 2001)}, "description_too_long"},
		{"screenshot not a data URL", map[string]any{"description": "x", "screenshot": "not-a-data-url"}, "screenshot_must_be_an_image"},
		{"screenshot disallowed mime type", map[string]any{"description": "x", "screenshot": "data:text/plain;base64,aGVsbG8="}, "screenshot_must_be_an_image"},
		{"screenshot missing base64 marker", map[string]any{"description": "x", "screenshot": "data:image/png,rawdata"}, "screenshot_invalid"},
		{"screenshot invalid base64 payload", map[string]any{"description": "x", "screenshot": "data:image/png;base64,not-valid-base64!!!"}, "screenshot_invalid"},
		{"screenshot exceeds size cap", map[string]any{"description": "x", "screenshot": "data:image/png;base64," + strings.Repeat("A", 7*1024*1024)}, "screenshot_too_large"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, resp := bugReportsSuiteDo(t, app, tc.payload)
			if status != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400, resp=%v", status, resp)
			}
			if resp["error"] != tc.wantError {
				t.Errorf("error = %v, want %s", resp["error"], tc.wantError)
			}
		})
	}
}

func TestBugReportsHandler_Create_RelaysToDiscord(t *testing.T) {
	var receivedForm *multipart.Form
	var receivedPayloadJSON string
	var receivedFileBytes []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("mock discord: parse multipart form: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receivedForm = r.MultipartForm
		receivedPayloadJSON = r.FormValue("payload_json")
		if f, fh, err := r.FormFile("files[0]"); err == nil {
			defer f.Close()
			buf := make([]byte, fh.Size)
			_, _ = f.Read(buf)
			receivedFileBytes = buf
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	app := bugReportsSuiteApp(config.Config{DiscordBugReportWebhookURL: server.URL})

	t.Run("without screenshot", func(t *testing.T) {
		receivedForm, receivedPayloadJSON, receivedFileBytes = nil, "", nil
		status, resp := bugReportsSuiteDo(t, app, map[string]any{
			"description":    "clicking Save on the settings page throws a blank white screen",
			"page_url":       "https://grainlify.com/dashboard?tab=settings",
			"reporter_login": "octocat",
		})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, resp=%v", status, resp)
		}
		if resp["ok"] != true {
			t.Errorf("ok = %v, want true", resp["ok"])
		}
		if receivedPayloadJSON == "" {
			t.Fatal("discord mock never received payload_json")
		}
		var embed struct {
			Embeds []struct {
				Description string `json:"description"`
				Fields      []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"fields"`
				Image *struct {
					URL string `json:"url"`
				} `json:"image"`
			} `json:"embeds"`
		}
		if err := json.Unmarshal([]byte(receivedPayloadJSON), &embed); err != nil {
			t.Fatalf("decode payload_json: %v", err)
		}
		if len(embed.Embeds) != 1 {
			t.Fatalf("embeds len = %d, want 1", len(embed.Embeds))
		}
		if !strings.Contains(embed.Embeds[0].Description, "blank white screen") {
			t.Errorf("embed description = %q, want it to contain the report text", embed.Embeds[0].Description)
		}
		if embed.Embeds[0].Image != nil {
			t.Errorf("image = %v, want nil (no screenshot was sent)", embed.Embeds[0].Image)
		}
		foundReporter := false
		for _, f := range embed.Embeds[0].Fields {
			if f.Name == "Reporter" && f.Value == "octocat" {
				foundReporter = true
			}
		}
		if !foundReporter {
			t.Errorf("fields = %+v, want a Reporter field with value octocat", embed.Embeds[0].Fields)
		}
		if receivedForm != nil && len(receivedForm.File) != 0 {
			t.Errorf("discord mock received a file part, want none: %+v", receivedForm.File)
		}
	})

	t.Run("with screenshot", func(t *testing.T) {
		receivedForm, receivedPayloadJSON, receivedFileBytes = nil, "", nil
		status, resp := bugReportsSuiteDo(t, app, map[string]any{
			"description": "the glare effect covers the whole card",
			"screenshot":  "data:image/png;base64," + bugReportsSuiteTinyPNG,
		})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, resp=%v", status, resp)
		}
		if receivedForm == nil || len(receivedForm.File["files[0]"]) != 1 {
			t.Fatalf("discord mock did not receive a files[0] part, form=%+v", receivedForm)
		}
		if len(receivedFileBytes) == 0 {
			t.Error("received screenshot bytes are empty")
		}
		var embed struct {
			Embeds []struct {
				Image *struct {
					URL string `json:"url"`
				} `json:"image"`
			} `json:"embeds"`
		}
		if err := json.Unmarshal([]byte(receivedPayloadJSON), &embed); err != nil {
			t.Fatalf("decode payload_json: %v", err)
		}
		if len(embed.Embeds) != 1 || embed.Embeds[0].Image == nil {
			t.Fatalf("embed image = %+v, want a non-nil attachment reference", embed.Embeds)
		}
		if embed.Embeds[0].Image.URL != "attachment://screenshot.png" {
			t.Errorf("image.url = %q, want attachment://screenshot.png", embed.Embeds[0].Image.URL)
		}
	})
}

func TestBugReportsHandler_Create_DiscordFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	app := bugReportsSuiteApp(config.Config{DiscordBugReportWebhookURL: server.URL})

	status, resp := bugReportsSuiteDo(t, app, map[string]any{"description": "something broke"})
	if status != fiber.StatusBadGateway {
		t.Fatalf("status = %d, want 502, resp=%v", status, resp)
	}
	if resp["error"] != "discord_relay_failed" {
		t.Errorf("error = %v, want discord_relay_failed", resp["error"])
	}
}
