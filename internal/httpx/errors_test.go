package httpx_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

func TestRespondError_WritesStableJSONEnvelope(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      string
		message   string
		requestID string
	}{
		{
			name:      "validation error",
			status:    fiber.StatusBadRequest,
			code:      "invalid_json",
			message:   "request body must be valid JSON",
			requestID: "req-validation",
		},
		{
			name:      "not found",
			status:    fiber.StatusNotFound,
			code:      "not_found",
			message:   "resource not found",
			requestID: "req-missing",
		},
		{
			name:      "internal error",
			status:    fiber.StatusInternalServerError,
			code:      "internal_server_error",
			message:   httpx.GenericInternalMessage,
			requestID: "req-internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := exerciseRespondError(t, tt.status, tt.code, tt.message, tt.requestID)
			defer resp.Body.Close()

			assert.Equal(t, tt.status, resp.StatusCode)
			assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))

			body := decodeRawErrorEnvelope(t, resp.Body)
			assert.Equal(t, map[string]any{
				"error":      tt.code,
				"message":    tt.message,
				"request_id": tt.requestID,
			}, body)
		})
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

func TestRespondError_NilOrEmptyInputsDoNotPanic(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		message   string
		requestID any
	}{
		{name: "empty message", code: "unauthorized", message: "", requestID: "req-empty-message"},
		{name: "empty code and message", code: "", message: "", requestID: "req-empty-fields"},
		{name: "nil request id local", code: "internal_server_error", message: "", requestID: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				app := fiber.New()
				app.Get("/test", func(c *fiber.Ctx) error {
					c.Locals("requestid", tt.requestID)
					return httpx.RespondError(c, fiber.StatusInternalServerError, httpx.Code(tt.code), tt.message)
				})

				resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
				require.NoError(t, err)
				defer resp.Body.Close()

				body := decodeRawErrorEnvelope(t, resp.Body)
				assert.Contains(t, body, "error")
				assert.Contains(t, body, "request_id")
			})
		})
	}
}

func TestRespondError_RealHandlerUsesSharedEnvelope(t *testing.T) {
	app := fiber.New()
	app.Use(requestid.New())
	app.Post("/auth/nonce", handlers.NewAuthHandler(config.Config{}, nil).Nonce())

	resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/auth/nonce", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))

	body := decodeRawErrorEnvelope(t, resp.Body)
	assert.Equal(t, "db_not_configured", body["error"])
	assert.Equal(t, httpx.GenericInternalMessage, body["message"])
	assert.NotEmpty(t, body["request_id"])
}

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
	if env.Message != httpx.GenericInternalMessage {
		t.Errorf("expected message to be forced to GenericInternalMessage, got %q", env.Message)
	}
}

func TestRespondError_5xxScrubsRawMessage(t *testing.T) {
	app := fiber.New()
	rawErrorMsg := "pq: relation users does not exist at character 14"
	app.Get("/test", func(c *fiber.Ctx) error {
		return httpx.RespondError(c, fiber.StatusInternalServerError, "db_error", rawErrorMsg)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &env))

	assert.Equal(t, "db_error", env.Error)
	assert.Equal(t, httpx.GenericInternalMessage, env.Message)
	assert.NotContains(t, string(body), rawErrorMsg)
}

func TestRespondError_5xxEmptyCodeDefaultsToCodeInternal(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		return httpx.RespondError(c, fiber.StatusServiceUnavailable, "", "some failure")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var env httpx.ErrorEnvelope
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &env))

	assert.Equal(t, string(httpx.CodeInternal), env.Error)
	assert.Equal(t, httpx.GenericInternalMessage, env.Message)
}

func TestDefaultCodeForStatus(t *testing.T) {
	tests := []struct {
		status   int
		expected httpx.Code
	}{
		{fiber.StatusBadRequest, httpx.CodeBadRequest},
		{fiber.StatusUnauthorized, httpx.CodeUnauthorized},
		{fiber.StatusForbidden, httpx.CodeForbidden},
		{fiber.StatusNotFound, httpx.CodeNotFound},
		{fiber.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed},
		{fiber.StatusConflict, httpx.CodeConflict},
		{fiber.StatusUnprocessableEntity, httpx.CodeUnprocessable},
		{fiber.StatusTooManyRequests, httpx.CodeTooManyRequests},
		{fiber.StatusRequestEntityTooLarge, httpx.CodeRequestTooLarge},
		{fiber.StatusServiceUnavailable, httpx.CodeServiceUnavailable},
		{fiber.StatusInternalServerError, httpx.CodeInternal},
		{999, httpx.CodeInternal},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.expected, httpx.DefaultCodeForStatus(tt.status))
		})
	}
}

func exerciseRespondError(t *testing.T, status int, code, message, requestID string) *http.Response {
	t.Helper()
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", requestID)
		return httpx.RespondError(c, status, httpx.Code(code), message)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	require.NoError(t, err)
	return resp
}

func decodeRawErrorEnvelope(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	payload, err := io.ReadAll(body)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded), "body: %s", payload)
	return decoded
}
