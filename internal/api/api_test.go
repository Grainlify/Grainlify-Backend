package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// ---------------------------------------------------------------------------
// This is the only test file in the internal/api package, so nothing here
// strictly needs a domain prefix to avoid collisions with concurrently
// written test files elsewhere - apiWiringSuite* is used anyway for
// consistency with the naming convention used across internal/handlers.
//
// None of the routes exercised here (root GET/POST, the catch-all 404, and
// CORS preflight handling) touch deps.DB or deps.Bus: every handler
// constructor api.New calls (handlers.NewXHandler, notifications.New,
// email.NewMailerCloudMailer) stores its *db.DB/bus.Bus argument without
// ever dereferencing it at construction time (verified by reading
// internal/api/api.go and the body of every handlers.NewXHandler
// constructor it calls - e.g. github_webhooks.go explicitly guards with
// "if d != nil && d.Pool != nil" before touching it), and the one handler
// that *does* need Pool.Ping at request time (Ready) is simply never
// requested by this suite. So the app is built with a zero-value
// api.Deps{} - no live Postgres needed, which keeps this suite runnable
// without TEST_DB_URL.
// ---------------------------------------------------------------------------

func apiWiringSuiteConfig() config.Config {
	return config.Config{JWTSecret: "api-wiring-suite-test-jwt-secret"}
}

func apiWiringSuiteApp(cfg config.Config) *fiber.App {
	return api.New(cfg, api.Deps{})
}

func apiWiringSuiteDo(t *testing.T, app *fiber.App, method, path string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return resp.StatusCode, decoded
}

// apiWiringSuiteCORSAllowOrigin issues a real CORS preflight - an OPTIONS
// request carrying both Origin and Access-Control-Request-Method, per the
// fetch spec - against path "/" and returns whatever the cors middleware
// set as Access-Control-Allow-Origin ("" if the origin was rejected).
//
// Per Fiber's cors middleware (gofiber/fiber/v2/middleware/cors/cors.go), a
// well-formed preflight always gets HTTP 204 back, origin allowed or not -
// setCORSHeaders only ever adds Access-Control-Allow-Origin when allowOrigin
// is non-empty, then the handler unconditionally does
// `return c.SendStatus(fiber.StatusNoContent)`. So the *only* signal of
// accept/reject is this response header, never the status code - this
// helper asserts the 204 as a sanity check and returns the header for the
// caller to make the real assertion.
func apiWiringSuiteCORSAllowOrigin(t *testing.T, app *fiber.App, origin string) string {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodOptions, "/", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", resp.StatusCode, fiber.StatusNoContent)
	}
	return resp.Header.Get("Access-Control-Allow-Origin")
}

func TestAPIRoot_GET(t *testing.T) {
	app := apiWiringSuiteApp(apiWiringSuiteConfig())
	status, body := apiWiringSuiteDo(t, app, fiber.MethodGet, "/")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
	}
	if body["service"] != "grainlify-api" {
		t.Errorf("service = %v, want grainlify-api", body["service"])
	}
	if body["status"] != "running" {
		t.Errorf("status field = %v, want running", body["status"])
	}
	if body["version"] != "1.0.0" {
		t.Errorf("version = %v, want 1.0.0", body["version"])
	}
}

func TestAPIRoot_POST_WebhookMisconfigured(t *testing.T) {
	app := apiWiringSuiteApp(apiWiringSuiteConfig())
	status, body := apiWiringSuiteDo(t, app, fiber.MethodPost, "/")
	if status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, fiber.StatusBadRequest)
	}
	if body["error"] != "webhook_url_misconfigured" {
		t.Errorf("error = %v, want webhook_url_misconfigured", body["error"])
	}
	if body["message"] != "Webhook requests should be sent to /webhooks/github, not /" {
		t.Errorf("message = %v, want the webhook-misconfigured message", body["message"])
	}
	if body["correct_url"] != "/webhooks/github" {
		t.Errorf("correct_url = %v, want /webhooks/github", body["correct_url"])
	}
}

func TestAPIUnmatchedRoute_404(t *testing.T) {
	app := apiWiringSuiteApp(apiWiringSuiteConfig())
	status, body := apiWiringSuiteDo(t, app, fiber.MethodGet, "/this-route-does-not-exist")
	if status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, fiber.StatusNotFound)
	}
	if body["error"] != "not_found" {
		t.Errorf("error = %v, want not_found", body["error"])
	}
	if body["path"] != "/this-route-does-not-exist" {
		t.Errorf("path = %v, want /this-route-does-not-exist", body["path"])
	}
}

func TestAPICORSAllowOriginsFunc(t *testing.T) {
	app := apiWiringSuiteApp(apiWiringSuiteConfig())

	cases := []struct {
		name        string
		origin      string
		wantAllowed bool
	}{
		{"http localhost with a port is allowed", "http://localhost:5173", true},
		{"https localhost with a port is allowed", "https://localhost:9999", true},
		{"http 127.0.0.1 with a port is allowed", "http://127.0.0.1:3000", true},
		{"https 127.0.0.1 with a port is allowed", "https://127.0.0.1:4000", true},
		{"vercel preview deployment is allowed", "https://grainlify-git-feature-branch.vercel.app", true},
		{"0xo.in production subdomain is allowed", "https://grainlify.0xo.in", true},
		{"api subdomain of 0xo.in is allowed", "https://api.grainlify.0xo.in", true},
		{"unrelated origin is rejected", "https://evil-phishing-site.example.com", false},
		{"lookalike suffix without a leading dot is rejected", "https://notvercel.app", false},
		// The AllowOriginsFunc suffix check is strictly "*.0xo.in" (must be
		// preceded by a literal dot); the bare apex domain has no such dot
		// before it, so it does NOT match on its own - consistent with the
		// api.go comment "Allow production domain (*.0xo.in) for
		// grainlify.0xo.in / api.grainlify.0xo.in" (subdomains only).
		{"bare 0xo.in apex domain (no subdomain) is rejected", "https://0xo.in", false},
		// Surfacing an actual behavior gap found while writing this test:
		// the localhost/127.0.0.1 rules in api.go match on the literal
		// prefix "http://localhost:" (colon included), so an Origin with
		// no port at all - which is exactly what a browser sends when the
		// frontend is served on the scheme's default port, e.g. plain
		// "http://localhost" on port 80 - does NOT match and is rejected.
		{"bare http://localhost with no port is rejected (prefix check requires a trailing colon+port)", "http://localhost", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := apiWiringSuiteCORSAllowOrigin(t, app, tc.origin)
			if tc.wantAllowed {
				if got != tc.origin {
					t.Errorf("Access-Control-Allow-Origin = %q, want %q (origin should be allowed)", got, tc.origin)
				}
			} else if got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want empty (origin should be rejected)", got)
			}
		})
	}
}

func TestAPICORSAllowOriginsFunc_ExplicitConfiguredOrigin(t *testing.T) {
	cfg := apiWiringSuiteConfig()
	cfg.CORSOrigins = "https://partner-a.example.com, https://partner-b.example.com"
	app := apiWiringSuiteApp(cfg)

	t.Run("an origin present in CORS_ORIGINS is allowed", func(t *testing.T) {
		got := apiWiringSuiteCORSAllowOrigin(t, app, "https://partner-b.example.com")
		if got != "https://partner-b.example.com" {
			t.Errorf("Access-Control-Allow-Origin = %q, want https://partner-b.example.com", got)
		}
	})

	t.Run("an origin absent from CORS_ORIGINS is still rejected", func(t *testing.T) {
		got := apiWiringSuiteCORSAllowOrigin(t, app, "https://not-configured.example.com")
		if got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})
}

func TestAPICORSAllowOriginsFunc_FrontendBaseURLFallback(t *testing.T) {
	cfg := apiWiringSuiteConfig()
	cfg.FrontendBaseURL = "https://app.grainlify-example.com/"
	app := apiWiringSuiteApp(cfg)

	t.Run("origin exactly matching the trimmed FrontendBaseURL is allowed", func(t *testing.T) {
		got := apiWiringSuiteCORSAllowOrigin(t, app, "https://app.grainlify-example.com")
		if got != "https://app.grainlify-example.com" {
			t.Errorf("Access-Control-Allow-Origin = %q, want https://app.grainlify-example.com", got)
		}
	})

	t.Run("unrelated origin is still rejected even with FrontendBaseURL configured", func(t *testing.T) {
		got := apiWiringSuiteCORSAllowOrigin(t, app, "https://unrelated.example.com")
		if got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})
}
