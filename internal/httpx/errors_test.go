package httpx_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			message:   "an unexpected error occurred",
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
				"error": map[string]any{
					"code":       tt.code,
					"message":    tt.message,
					"request_id": tt.requestID,
				},
			}, body)
		})
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
					return httpx.RespondError(c, fiber.StatusInternalServerError, tt.code, tt.message)
				})

				resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
				require.NoError(t, err)
				defer resp.Body.Close()

				body := decodeRawErrorEnvelope(t, resp.Body)
				errorBody := requireErrorObject(t, body)
				assert.Contains(t, errorBody, "code")
				assert.Contains(t, errorBody, "message")
				assert.Contains(t, errorBody, "request_id")
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
	assert.Equal(t, map[string]any{
		"error": map[string]any{
			"code":       "db_not_configured",
			"message":    "",
			"request_id": requireErrorObject(t, body)["request_id"],
		},
	}, body)
	assert.NotEmpty(t, requireErrorObject(t, body)["request_id"])
}

func exerciseRespondError(t *testing.T, status int, code, message, requestID string) *http.Response {
	t.Helper()
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		c.Locals("requestid", requestID)
		return httpx.RespondError(c, status, code, message)
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

func requireErrorObject(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errorValue, ok := body["error"]
	require.True(t, ok, "missing top-level error field")
	errorBody, ok := errorValue.(map[string]any)
	require.True(t, ok, "error field must be an object")
	return errorBody
}
