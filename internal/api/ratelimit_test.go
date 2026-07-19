package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/jagadeesh/grainlify/backend/internal/config"
)

func TestRateLimitMiddleware_AuthRoutesPerIP(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   2,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := newRateLimitTestApp(cfg, "/auth/nonce")

	for i := 0; i < 2; i++ {
		status := rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.10", "")
		if status != fiber.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, status, fiber.StatusOK)
		}
	}

	status := rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.10", "")
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("exceeding request status = %d, want %d", status, fiber.StatusTooManyRequests)
	}
}

func TestRateLimitMiddleware_PublicRoutesUseHigherBucket(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   2,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := newRateLimitTestApp(cfg, "/projects")

	for i := 0; i < 3; i++ {
		status := rateLimitTestRequest(t, app, "/projects", "203.0.113.20", "")
		if status != fiber.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, status, fiber.StatusOK)
		}
	}
}

func TestRateLimitMiddleware_TrustedProxyForwardedFor(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin:   1,
		RateLimitPublicPerMin: 3,
		TrustedProxies:        []string{"127.0.0.1"},
	}

	app := newRateLimitTestApp(cfg, "/auth/nonce")

	firstStatus := rateLimitTestRequest(t, app, "/auth/nonce", "127.0.0.1", "198.51.100.10")
	if firstStatus != fiber.StatusOK {
		t.Fatalf("first request status = %d, want %d", firstStatus, fiber.StatusOK)
	}

	secondStatus := rateLimitTestRequest(t, app, "/auth/nonce", "127.0.0.1", "198.51.100.11")
	if secondStatus != fiber.StatusOK {
		t.Fatalf("second request status = %d, want %d", secondStatus, fiber.StatusOK)
	}
}

func TestRateLimitMiddleware_BurstThenRefillWithoutSleeping(t *testing.T) {
	const (
		limit         = 2
		baseTimestamp = uint32(1_900_000_000)
	)

	cfg := config.Config{
		RateLimitAuthPerMin: limit,
		TrustedProxies:      []string{"127.0.0.1"},
	}
	app := newRateLimitTestApp(cfg, "/auth/nonce")

	for i := 0; i < limit; i++ {
		setRateLimitTestTimestamp(baseTimestamp)
		status := rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.30", "")
		if status != fiber.StatusOK {
			t.Fatalf("request %d inside burst status = %d, want %d", i+1, status, fiber.StatusOK)
		}
	}

	setRateLimitTestTimestamp(baseTimestamp)
	status := rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.30", "")
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("request immediately after burst status = %d, want %d", status, fiber.StatusTooManyRequests)
	}

	setRateLimitTestTimestamp(baseTimestamp + 59)
	status = rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.30", "")
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("request one second before refill status = %d, want %d", status, fiber.StatusTooManyRequests)
	}

	setRateLimitTestTimestamp(baseTimestamp + 60)
	status = rateLimitTestRequest(t, app, "/auth/nonce", "203.0.113.30", "")
	if status != fiber.StatusOK {
		t.Fatalf("request exactly at refill boundary status = %d, want %d", status, fiber.StatusOK)
	}
}

func TestRateLimitMiddleware_PerKeyIsolation(t *testing.T) {
	cfg := config.Config{
		RateLimitAuthPerMin: 1,
		TrustedProxies:      []string{"127.0.0.1"},
	}
	app := newRateLimitTestApp(cfg, "/auth/nonce")
	setRateLimitTestTimestamp(1_900_000_100)

	firstClientStatus := rateLimitTestRequest(t, app, "/auth/nonce", "127.0.0.1", "203.0.113.40")
	if firstClientStatus != fiber.StatusOK {
		t.Fatalf("first client initial request status = %d, want %d", firstClientStatus, fiber.StatusOK)
	}

	firstClientExceededStatus := rateLimitTestRequest(t, app, "/auth/nonce", "127.0.0.1", "203.0.113.40")
	if firstClientExceededStatus != fiber.StatusTooManyRequests {
		t.Fatalf("first client second request status = %d, want %d", firstClientExceededStatus, fiber.StatusTooManyRequests)
	}

	secondClientStatus := rateLimitTestRequest(t, app, "/auth/nonce", "127.0.0.1", "203.0.113.41")
	if secondClientStatus != fiber.StatusOK {
		t.Fatalf("second client initial request status = %d, want %d", secondClientStatus, fiber.StatusOK)
	}
}

func TestRateLimitMiddleware_ExactConfiguredLimitBoundary(t *testing.T) {
	const limit = 3

	cfg := config.Config{
		RateLimitPublicPerMin: limit,
		TrustedProxies:        []string{"127.0.0.1"},
	}
	app := newRateLimitTestApp(cfg, "/projects")
	setRateLimitTestTimestamp(1_900_000_200)

	for i := 0; i < limit; i++ {
		status := rateLimitTestRequest(t, app, "/projects", "203.0.113.50", "")
		if status != fiber.StatusOK {
			t.Fatalf("request %d of configured limit status = %d, want %d", i+1, status, fiber.StatusOK)
		}
	}

	status := rateLimitTestRequest(t, app, "/projects", "203.0.113.50", "")
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("request %d over configured limit status = %d, want %d", limit+1, status, fiber.StatusTooManyRequests)
	}
}

func newRateLimitTestApp(cfg config.Config, route string) *fiber.App {
	app := fiber.New(fiber.Config{
		ProxyHeader:             fiber.HeaderXForwardedFor,
		EnableTrustedProxyCheck: true,
		TrustedProxies:          cfg.TrustedProxies,
	})
	app.Use(NewRateLimitMiddleware(cfg))
	app.Get(route, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	return app
}

func rateLimitTestRequest(t *testing.T, app *fiber.App, path, remoteIP, forwardedFor string) int {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = fmt.Sprintf("%s:12345", remoteIP)
	if forwardedFor != "" {
		req.Header.Set(fiber.HeaderXForwardedFor, forwardedFor)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp.StatusCode
}

func setRateLimitTestTimestamp(ts uint32) {
	atomic.StoreUint32(&utils.Timestamp, ts)
}
