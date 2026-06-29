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

	if env.Error.Code != "invalid_json" {
		t.Errorf("expected code 'invalid_json', got %q", env.Error.Code)
	}
	if env.Error.Message != "request body must be valid JSON" {
		t.Errorf("unexpected message: %q", env.Error.Message)
	}
	if env.Error.RequestID != "req-abc-123" {
		t.Errorf("expected request_id 'req-abc-123', got %q", env.Error.RequestID)
	}
}

func TestRespondError_MissingRequestID(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		// requestid local not set – simulates middleware absence
		return httpx.RespondError(c, fiber.StatusInternalServerError, "internal_error", "an unexpected error occurred")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}

	// request_id must be empty string (not missing) so the key is always present
	if env.Error.RequestID != "" {
		t.Errorf("expected empty request_id, got %q", env.Error.RequestID)
	}
	if env.Error.Code != "internal_error" {
		t.Errorf("expected code 'internal_error', got %q", env.Error.Code)
	}
}

func TestRespondError_404NotFound(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", "rid-xyz")
		return httpx.RespondError(c, fiber.StatusNotFound, "not_found", "resource not found")
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
	if env.Error.Code != "not_found" {
		t.Errorf("expected 'not_found', got %q", env.Error.Code)
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
	var raw map[string]map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("failed to parse JSON: %v\nbody: %s", err, body)
	}
	inner, ok := raw["error"]
	if !ok {
		t.Fatal("top-level 'error' key missing")
	}
	for _, key := range []string{"code", "message", "request_id"} {
		if _, exists := inner[key]; !exists {
			t.Errorf("missing key %q in error envelope", key)
		}
	}
}
