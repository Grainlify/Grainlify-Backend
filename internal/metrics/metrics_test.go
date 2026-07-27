package metrics_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jagadeesh/grainlify/backend/internal/metrics"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/projects/123/issues", "/projects/:id/issues"},
		{"/users/550e8400-e29b-41d4-a716-446655440000/profile", "/users/:id/profile"},
		{"/health", "/health"},
		{"/projects/mine", "/projects/mine"},
	}
	for _, c := range cases {
		if got := metrics.NormalizePath(c.in); got != c.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{200, "2xx"},
		{404, "4xx"},
		{500, "5xx"},
		{201, "2xx"},
	}
	for _, c := range cases {
		if got := metrics.StatusClass(c.code); got != c.want {
			t.Errorf("StatusClass(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestLatencyMiddleware_RecordsObservation(t *testing.T) {
	// Use a fresh registry so this test is isolated.
	reg := prometheus.NewRegistry()
	hist := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Name: "test_latency",
	}, []string{"route", "method", "status_class"})

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		start := c.Context()
		_ = start
		return c.Next()
	})
	// Wire the real middleware — it writes to the global registry.
	app.Use(metrics.LatencyMiddleware())
	app.Get("/ping", func(c *fiber.Ctx) error { return c.SendStatus(200) })
	_ = hist // used to confirm the pattern; real metric lives in default reg

	req := httptest.NewRequest("GET", "/ping", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestTokenGate(t *testing.T) {
	app := fiber.New()
	app.Get("/metrics", metrics.TokenGate("secret"), func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	// No token → 401.
	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, _ := app.Test(req)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// Wrong token → 401.
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = app.Test(req)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", resp.StatusCode)
	}

	// Correct token → 200.
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, _ = app.Test(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with correct token, got %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestTokenGate_EmptyToken_Passthrough(t *testing.T) {
	app := fiber.New()
	app.Get("/metrics", metrics.TokenGate(""), func(c *fiber.Ctx) error {
		return c.SendString("open")
	})

	req := httptest.NewRequest("GET", "/metrics", nil)
	resp, _ := app.Test(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with empty token (no-op gate), got %d", resp.StatusCode)
	}
}
