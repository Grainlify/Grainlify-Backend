package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/config"
)

func TestRateLimitMiddleware_AuthRoutesPerIP(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   2,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := fiber.New(fiber.Config{
		ProxyHeader:             fiber.HeaderXForwardedFor,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          cfg.TrustedProxies,
	})
	app.Use(NewRateLimitMiddleware(cfg))
	app.Get("/auth/nonce", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/auth/nonce", nil)
		req.RemoteAddr = "203.0.113.10:12345"
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, resp.StatusCode, fiber.StatusOK)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/nonce", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("exceeding request failed: %v", err)
	}
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("exceeding request status = %d, want %d", resp.StatusCode, fiber.StatusTooManyRequests)
	}
}

func TestRateLimitMiddleware_PublicRoutesUseHigherBucket(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   2,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := fiber.New(fiber.Config{
		ProxyHeader:             fiber.HeaderXForwardedFor,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          cfg.TrustedProxies,
	})
	app.Use(NewRateLimitMiddleware(cfg))
	app.Get("/projects", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.RemoteAddr = "203.0.113.20:12345"
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, resp.StatusCode, fiber.StatusOK)
		}
	}
}

func TestRateLimitMiddleware_TrustedProxyForwardedFor(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   1,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := fiber.New(fiber.Config{
		ProxyHeader:             fiber.HeaderXForwardedFor,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          cfg.TrustedProxies,
	})
	app.Use(NewRateLimitMiddleware(cfg))
	app.Get("/auth/nonce", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	firstReq := httptest.NewRequest(http.MethodGet, "/auth/nonce", nil)
	firstReq.RemoteAddr = "127.0.0.1:12345"
	firstReq.Header.Set("X-Forwarded-For", "198.51.100.10")
	firstResp, err := app.Test(firstReq)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if firstResp.StatusCode != fiber.StatusOK {
		t.Fatalf("first request status = %d, want %d", firstResp.StatusCode, fiber.StatusOK)
	}

	secondReq := httptest.NewRequest(http.MethodGet, "/auth/nonce", nil)
	secondReq.RemoteAddr = "127.0.0.1:12345"
	secondReq.Header.Set("X-Forwarded-For", "198.51.100.11")
	secondResp, err := app.Test(secondReq)
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if secondResp.StatusCode != fiber.StatusOK {
		t.Fatalf("second request status = %d, want %d", secondResp.StatusCode, fiber.StatusOK)
	}
}
