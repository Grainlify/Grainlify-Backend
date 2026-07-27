package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBodyLimitMiddleware(t *testing.T) {
	cfg := config.Config{
		MaxBodyBytes:        100,
		WebhookMaxBodyBytes: 10 * 1024 * 1024,
		GitHubWebhookSecret: "",
	}
	app := api.New(cfg, api.Deps{}, handlers.BuildInfo{})

	t.Run("under_limit_passes", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 50))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)

		// Root POST returns 400 (misconfigured webhook handler), not 413
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "webhook_url_misconfigured", res["error"])
	})

	t.Run("at_limit_passes", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 100))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "webhook_url_misconfigured", res["error"])
	})

	t.Run("one_byte_over_returns_413", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 101))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "request_entity_too_large", res["error"])
		assert.Equal(t, "Request body exceeds size limit", res["message"])
		assert.NotEmpty(t, res["request_id"])
	})

	t.Run("oversized_returns_413", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 200))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "request_entity_too_large", res["error"])
		assert.NotEmpty(t, res["request_id"])
	})

	t.Run("get_method_not_limited", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 200))
		req := httptest.NewRequest(http.MethodGet, "/health", bytes.NewReader(body))

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)
		// GET requests bypass body limit, should reach handler
		assert.NotEqual(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})

	t.Run("webhook_github_bypasses_standard_limit", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 5000))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)

		// Should bypass the 100-byte standard limit and reach the webhook handler.
		// With empty GitHubWebhookSecret, the handler returns 503.
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "webhook_secret_not_configured", res["error"])
	})

	t.Run("webhook_didit_bypasses_standard_limit", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 5000))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/didit", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)

		// Should bypass the 100-byte standard limit and reach the handler.
		// Just verify it's NOT 413 — the actual status depends on the handler.
		assert.NotEqual(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})

	t.Run("webhook_github_exceeds_global_limit", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 11*1024*1024))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		_, err := app.Test(req, 30000)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "body size exceeds the given limit")
	})

	t.Run("error_envelope_shape", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 101))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)

		_, hasError := res["error"]
		_, hasRequestID := res["request_id"]
		assert.True(t, hasError, "response must contain 'error' field")
		assert.True(t, hasRequestID, "response must contain 'request_id' field")
		assert.Equal(t, "request_entity_too_large", res["error"])
	})
}

func TestBodyLimitMiddleware_ZeroLimit(t *testing.T) {
	cfg := config.Config{
		MaxBodyBytes:        0,
		WebhookMaxBodyBytes: 0,
	}
	app := api.New(cfg, api.Deps{}, handlers.BuildInfo{})

	body := []byte(strings.Repeat("a", 1_000_000))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, 30000)
	require.NoError(t, err)

	// Zero limit means no body size enforcement; should reach the POST / handler (400), not 413.
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
