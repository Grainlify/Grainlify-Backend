package handlers_test

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/soroban"
)

func TestLoggingRedaction(t *testing.T) {
	var buf bytes.Buffer
	// Capture INFO level logs
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	t.Run("Soroban_LogContractInteraction_RedactsInfo", func(t *testing.T) {
		buf.Reset()

		cfg := soroban.Config{
			RPCURL:  "http://localhost:8000",
			Network: soroban.NetworkTestnet,
		}
		client, err := soroban.NewClient(cfg)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		args := map[string]interface{}{
			"address": "GDR5...XYZ",
			"amount":  1000,
			"other":   "safe_value",
		}

		client.LogContractInteraction("contract123", "deposit", args)

		output := buf.String()
		if !strings.Contains(output, "contract interaction") {
			t.Errorf("expected 'contract interaction' in log, got: %s", output)
		}
		if strings.Contains(output, "GDR5...XYZ") {
			t.Errorf("expected address to be redacted, got: %s", output)
		}
		if strings.Contains(output, "1000") {
			t.Errorf("expected amount to be redacted, got: %s", output)
		}
		if !strings.Contains(output, "[REDACTED]") {
			t.Errorf("expected [REDACTED] placeholder in log, got: %s", output)
		}
		if !strings.Contains(output, "safe_value") {
			t.Errorf("expected safe_value to be retained in log, got: %s", output)
		}
	})

	t.Run("GitHubWebhooks_NoBodyAtInfo", func(t *testing.T) {
		buf.Reset()

		app := fiber.New()
		cfg := config.Config{GitHubWebhookSecret: "secret123"}
		
		// Use empty DB and Bus since we just want to test early handler logging
		ghHandler := handlers.NewGitHubWebhooksHandler(cfg, nil, nil)
		app.Post("/webhook", ghHandler.Receive())

		bodyStr := `{"action": "opened", "secret_payload": "my-super-secret-token"}`
		req := httptest.NewRequest("POST", "/webhook", bytes.NewReader([]byte(bodyStr)))
		req.Header.Set("X-GitHub-Delivery", "12345")
		req.Header.Set("X-GitHub-Event", "issues")
		req.Header.Set("Content-Type", "application/json")

		_, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("app.Test failed: %v", err)
		}

		output := buf.String()
		if strings.Contains(output, "secret_payload") {
			t.Errorf("expected webhook body to be absent from INFO logs, got: %s", output)
		}
		if strings.Contains(output, "my-super-secret-token") {
			t.Errorf("expected webhook body to be absent from INFO logs, got: %s", output)
		}
		if !strings.Contains(output, "GitHub Webhook Request Received") {
			t.Errorf("expected concise info log, got: %s", output)
		}
	})
}
