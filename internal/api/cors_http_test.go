package api_test

// These tests exercise the CORS middleware over real HTTP requests, mirroring
// exactly how api.New wires cors.New(corsConfig) with AllowOriginsFunc and
// AllowCredentials: true. cors_test.go (package api) already covers
// CORSOriginPolicy.Allows in isolation; these tests instead assert on the
// actual Access-Control-Allow-Origin / -Credentials response headers, so a
// future edit can't silently reintroduce a wildcard-reflection or
// credentials+wildcard misconfiguration in how the policy is wired up.

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// newCORSTestApp wires cors.New with the same AllowOriginsFunc/AllowCredentials
// shape api.New uses, without needing a full App (DB, bus, etc).
func newCORSTestApp(cfg config.Config) *fiber.App {
	app := fiber.New()
	policy := api.BuildCORSOriginPolicy(cfg)
	app.Use(cors.New(cors.Config{
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization, X-Admin-Bootstrap-Token",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowCredentials: true,
		AllowOriginsFunc: policy.Allows,
	}))
	app.Get("/ping", func(c *fiber.Ctx) error { return c.SendString("pong") })
	return app
}

func doCORSRequest(t *testing.T, app *fiber.App, origin string) *corsResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/ping", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	return &corsResponse{
		AllowOrigin:      resp.Header.Get("Access-Control-Allow-Origin"),
		AllowCredentials: resp.Header.Get("Access-Control-Allow-Credentials"),
		StatusCode:       resp.StatusCode,
	}
}

// corsResponse captures just the headers these tests care about.
type corsResponse struct {
	AllowOrigin      string
	AllowCredentials string
	StatusCode       int
}

func TestCORSMiddleware_AllowlistedOriginGetsExactEcho(t *testing.T) {
	app := newCORSTestApp(config.Config{
		Env:             "production",
		CORSOrigins:     "https://grainlify.0xo.in",
		FrontendBaseURL: "https://grainlify.0xo.in",
	})

	resp := doCORSRequest(t, app, "https://grainlify.0xo.in")

	assert.Equal(t, "https://grainlify.0xo.in", resp.AllowOrigin)
	assert.Equal(t, "true", resp.AllowCredentials)
}

func TestCORSMiddleware_NonAllowlistedOriginGetsNoAllowOriginHeader(t *testing.T) {
	app := newCORSTestApp(config.Config{
		Env:             "production",
		CORSOrigins:     "https://grainlify.0xo.in",
		FrontendBaseURL: "https://grainlify.0xo.in",
	})

	resp := doCORSRequest(t, app, "https://attacker.example.com")

	assert.Empty(t, resp.AllowOrigin, "a non-allowlisted origin must not be reflected back")
	assert.Empty(t, resp.AllowCredentials, "credentials header must not be sent for a denied origin")
}

func TestCORSMiddleware_DoesNotReflectArbitraryOrigins(t *testing.T) {
	app := newCORSTestApp(config.Config{
		Env:             "production",
		CORSOrigins:     "https://grainlify.0xo.in",
		FrontendBaseURL: "https://grainlify.0xo.in",
	})

	untrusted := []string{
		"https://grainlify.0xo.in.attacker.com",
		"null",
		"https://evil.com",
		"http://localhost:5173",
	}
	for _, origin := range untrusted {
		resp := doCORSRequest(t, app, origin)
		assert.Empty(t, resp.AllowOrigin, "origin %q must not be reflected", origin)
	}
}

func TestCORSMiddleware_NeverPairsWildcardWithCredentials(t *testing.T) {
	// Covers every config shape this policy can produce, including the
	// permissive dev and preview-wildcard modes, so a future change to
	// BuildCORSOriginPolicy or api.New can't silently start pairing a
	// wildcard Access-Control-Allow-Origin with credentials=true — the
	// classic reflected-origin credential-theft misconfiguration.
	configs := map[string]config.Config{
		"production":       {Env: "production", CORSOrigins: "https://grainlify.0xo.in", FrontendBaseURL: "https://grainlify.0xo.in"},
		"dev":              {Env: "dev", CORSOrigins: "http://localhost:5173", FrontendBaseURL: "http://localhost:5173"},
		"preview-wildcard": {Env: "production", CORSOrigins: "https://grainlify.0xo.in", FrontendBaseURL: "https://grainlify.0xo.in", CORSAllowPreview: true},
	}
	origins := []string{
		"https://grainlify.0xo.in",
		"http://localhost:5173",
		"https://preview-branch.vercel.app",
		"https://attacker.example.com",
		"*",
	}

	for name, cfg := range configs {
		app := newCORSTestApp(cfg)
		for _, origin := range origins {
			resp := doCORSRequest(t, app, origin)
			if resp.AllowOrigin == "*" && resp.AllowCredentials == "true" {
				t.Fatalf("%s: origin %q produced wildcard+credentials, a credential-theft misconfiguration", name, origin)
			}
		}
	}
}

func TestCORSMiddleware_NoOriginHeaderGetsNoAllowOriginHeader(t *testing.T) {
	app := newCORSTestApp(config.Config{
		Env:             "production",
		CORSOrigins:     "https://grainlify.0xo.in",
		FrontendBaseURL: "https://grainlify.0xo.in",
	})

	resp := doCORSRequest(t, app, "")

	assert.Empty(t, resp.AllowOrigin)
}
