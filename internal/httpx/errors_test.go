package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

func TestRespondError_BasicEnvelope(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", "req-abc-123")
		return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "request body must be valid JSON")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}

	if env.Error != "invalid_json" {
		t.Errorf("expected code invalid_json, got %q", env.Error)
	}
	if env.Message != "request body must be valid JSON" {
		t.Errorf("unexpected message: %q", env.Message)
	}
	if env.RequestID != "req-abc-123" {
		t.Errorf("expected request_id req-abc-123, got %q", env.RequestID)
	}
}

func TestRespondError_MissingRequestID(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		return httpx.RespondError(c, fiber.StatusBadRequest, "bad_request", "missing field")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}

	if env.RequestID != "" {
		t.Errorf("expected empty request_id, got %q", env.RequestID)
	}
	if env.Error != "bad_request" {
		t.Errorf("expected code bad_request, got %q", env.Error)
	}
}

func TestRespondError_404NotFound(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", "rid-xyz")
		return httpx.RespondError(c, fiber.StatusNotFound, httpx.CodeNotFound, "resource not found")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if env.Error != "not_found" {
		t.Errorf("expected not_found, got %q", env.Error)
	}
}

func TestRespondError_JSONShape(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", "shape-test")
		return httpx.RespondError(c, fiber.StatusUnauthorized, "unauthorized", "")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}
	for _, key := range []string{"error", "request_id"} {
		if _, exists := raw[key]; !exists {
			t.Errorf("missing key %q in error envelope", key)
		}
	}
}

// RespondError does not auto-scrub 5xx responses -- it is the caller's
// responsibility to only ever pass a static, developer-chosen code/message
// (never a raw error). This test locks in that pass-through contract so a
// future change doesn't silently start scrubbing codes that handlers rely
// on (e.g. "nonce_create_failed", "db_not_configured").
func TestRespondError_5xxPassesThroughStaticCode(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		return httpx.RespondError(c, fiber.StatusInternalServerError, "nonce_create_failed", "")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}
	if env.Error != "nonce_create_failed" {
		t.Errorf("expected code to pass through unchanged, got %q", env.Error)
	}
}
