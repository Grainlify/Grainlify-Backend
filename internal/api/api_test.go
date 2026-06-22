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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestBodySizeLimit(t *testing.T) {
	// Initialize API config with a small MaxBodyBytes limit of 100 bytes
	cfg := config.Config{
		MaxBodyBytes:        100,
		GitHubWebhookSecret: "", // leaves webhook unconfigured to trigger 503 on handler success
	}

	app := api.New(cfg, api.Deps{})

	t.Run("StandardRoute_UnderLimit_Succeeds", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 50))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000) // Use 30s timeout to prevent system load timeouts
		require.NoError(t, err)

		// Root POST returns 400 Bad Request because of misconfigured webhook,
		// but it should NOT return 413 Request Entity Too Large since body is under 100 bytes.
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "webhook_url_misconfigured", res["error"])
	})

	t.Run("StandardRoute_AtLimit_Succeeds", func(t *testing.T) {
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

	t.Run("StandardRoute_JustOverLimit_FailsWith413", func(t *testing.T) {
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
		assert.Equal(t, "Request Entity Too Large", res["message"])
		assert.NotEmpty(t, res["request_id"])
	})

	t.Run("StandardRoute_Oversized_FailsWith413", func(t *testing.T) {
		body := []byte(strings.Repeat("a", 150))
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)

		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "request_entity_too_large", res["error"])
		assert.Equal(t, "Request Entity Too Large", res["message"])
		assert.NotEmpty(t, res["request_id"])
	})

	t.Run("WebhookRoute_BypassesStandardLimit", func(t *testing.T) {
		// Send 5000 bytes (well over the 100 bytes standard limit but below the 10 MB global limit)
		body := []byte(strings.Repeat("a", 5000))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req, 30000)
		require.NoError(t, err)

		// It should bypass the body limit middleware and reach the webhook handler.
		// Since the secret is not configured, it will return 503 Service Unavailable, NOT 413.
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

		var res map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&res)
		require.NoError(t, err)
		assert.Equal(t, "webhook_secret_not_configured", res["error"])
	})

	t.Run("WebhookRoute_ExceedsWebhookLimit_FailsWith413", func(t *testing.T) {
		// Send 11 MB (exceeds the 10 MB global/webhook limit)
		body := []byte(strings.Repeat("a", 11*1024*1024))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		// fiber.App.Test checks the global body limit client-side and returns an error immediately.
		_, err := app.Test(req, 30000)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "body size exceeds the given limit")
	})
}
