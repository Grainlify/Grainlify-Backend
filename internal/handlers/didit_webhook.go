package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

const (
	diditWebhookSignatureHeader  = "X-Signature"
	diditWebhookTimestampHeader  = "X-Timestamp"
	diditWebhookMaxTimestampSkew = 5 * time.Minute
)

type diditDecisionClient interface {
	GetSessionDecision(ctx context.Context, sessionID string) (didit.SessionDecisionResponse, error)
}

var errDiditAPIClientNotConfigured = errors.New("didit api client not configured")

type DiditWebhookHandler struct {
	cfg   config.Config
	db    *db.DB
	didit diditDecisionClient
}

func NewDiditWebhookHandler(cfg config.Config, d *db.DB) *DiditWebhookHandler {
	var diditClient diditDecisionClient
	if cfg.DiditAPIKey != "" {
		diditClient = didit.NewClient(cfg.DiditAPIKey)
	}
	return &DiditWebhookHandler{
		cfg:   cfg,
		db:    d,
		didit: diditClient,
	}
}

// WebhookEvent represents a Didit webhook event.
type WebhookEvent struct {
	EventID     string                 `json:"event_id"`
	Event       string                 `json:"event"`
	WebhookType string                 `json:"webhook_type"`
	SessionID   string                 `json:"session_id"`
	Status      string                 `json:"status,omitempty"`
	Timestamp   int64                  `json:"timestamp,omitempty"`
	Data        map[string]interface{} `json:"data,omitempty"`
	Decision    map[string]interface{} `json:"decision,omitempty"`
}

// Receive handles incoming Didit webhook events and callback redirects.
func (h *DiditWebhookHandler) Receive() fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodPost:
			return h.handleWebhook(c)
		case fiber.MethodGet:
			if h.db == nil || h.db.Pool == nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
			}
			return h.handleCallback(c)
		default:
			return c.Status(fiber.StatusMethodNotAllowed).JSON(fiber.Map{"error": "method_not_allowed"})
		}
	}
}

func (h *DiditWebhookHandler) handleWebhook(c *fiber.Ctx) error {
	body := c.Body()
	signature := strings.TrimSpace(c.Get(diditWebhookSignatureHeader))
	timestamp := strings.TrimSpace(c.Get(diditWebhookTimestampHeader))

	slog.Info("Didit webhook request received",
		"path", c.Path(),
		"remote_ip", c.IP(),
		"body_size_bytes", len(body),
		"signature_present", signature != "",
		"timestamp_present", timestamp != "",
	)

	if strings.TrimSpace(h.cfg.DiditWebhookSecret) == "" {
		slog.Error("Didit webhook secret not configured - rejecting request",
			"path", c.Path(),
			"remote_ip", c.IP(),
		)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "webhook_secret_not_configured"})
	}

	if !verifyDiditSignature(h.cfg.DiditWebhookSecret, body, signature, timestamp) {
		slog.Warn("Didit webhook signature verification failed",
			"path", c.Path(),
			"remote_ip", c.IP(),
			"body_size", len(body),
			"signature_present", signature != "",
			"timestamp_present", timestamp != "",
		)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_signature"})
	}

	slog.Info("Didit webhook signature verification succeeded",
		"path", c.Path(),
		"remote_ip", c.IP(),
	)

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_json"})
	}

	slog.Info("Didit webhook event parsed",
		"event_id", event.EventID,
		"event", event.Event,
		"webhook_type", event.WebhookType,
		"session_id", event.SessionID,
		"status", event.Status,
		"timestamp", event.Timestamp,
	)

	if event.SessionID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_session_id"})
	}
	if h.db == nil || h.db.Pool == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
	}

	userID, err := h.lookupUserByKYCSessionID(c.Context(), event.SessionID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session_not_found"})
	}

	kycStatus, decisionJSON, err := h.resolveDiditStatus(c.Context(), event.SessionID, event.Status, false)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "didit_decision_fetch_failed"})
	}

	if err := h.updateUserKYCStatus(c.Context(), userID, kycStatus, decisionJSON); err != nil {
		slog.Error("failed to update kyc status", "error", err, "user_id", userID, "status", kycStatus)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "kyc_update_failed"})
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "status": kycStatus})
}

func (h *DiditWebhookHandler) handleCallback(c *fiber.Ctx) error {
	sessionID := strings.TrimSpace(c.Query("verificationSessionId"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.Query("session_id"))
	}
	if sessionID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_session_id"})
	}

	slog.Info("Didit callback received", "session_id", sessionID)

	kycStatus, decisionJSON, err := h.resolveDiditStatus(c.Context(), sessionID, "", true)
	if err != nil {
		slog.Error("didit callback decision fetch failed", "session_id", sessionID, "error", err.Error())
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "didit_decision_fetch_failed"})
	}

	userID, err := h.lookupUserByKYCSessionID(c.Context(), sessionID)
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session_not_found"})
	}

	if err := h.updateUserKYCStatus(c.Context(), userID, kycStatus, decisionJSON); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "kyc_update_failed"})
	}

	redirectURL := h.diditCallbackRedirectURL(kycStatus, sessionID)
	if redirectURL != "" {
		return c.Redirect(redirectURL, fiber.StatusFound)
	}

	return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "status": kycStatus})
}

func (h *DiditWebhookHandler) lookupUserByKYCSessionID(ctx context.Context, sessionID string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := h.db.Pool.QueryRow(ctx, `
SELECT id
FROM users
WHERE kyc_session_id = $1
`, sessionID).Scan(&userID)
	return userID, err
}

func (h *DiditWebhookHandler) resolveDiditStatus(ctx context.Context, sessionID, fallbackStatus string, requireAPI bool) (string, []byte, error) {
	if h.didit == nil {
		if requireAPI {
			return "", nil, errDiditAPIClientNotConfigured
		}
		return mapDiditStatus(fallbackStatus), nil, nil
	}

	decision, err := h.didit.GetSessionDecision(ctx, sessionID)
	if err != nil {
		if requireAPI {
			return "", nil, err
		}
		slog.Warn("didit api call failed; using signed webhook status",
			"session_id", sessionID,
			"error", err.Error(),
			"fallback_status", fallbackStatus,
		)
		return mapDiditStatus(fallbackStatus), nil, nil
	}

	decisionData := diditDecisionData(decision)
	decisionJSON, _ := json.Marshal(decisionData)
	return mapDiditStatus(decision.Status), decisionJSON, nil
}

func (h *DiditWebhookHandler) updateUserKYCStatus(ctx context.Context, userID uuid.UUID, kycStatus string, decisionJSON []byte) error {
	if len(decisionJSON) == 0 {
		decisionJSON = []byte("{}")
	}

	_, err := h.db.Pool.Exec(ctx, `
UPDATE users
SET kyc_status = $1,
    kyc_data = $2,
    kyc_verified_at = CASE WHEN $1 = 'verified' THEN now() ELSE kyc_verified_at END,
    updated_at = now()
WHERE id = $3
`, kycStatus, decisionJSON, userID)
	return err
}

func (h *DiditWebhookHandler) diditCallbackRedirectURL(kycStatus, sessionID string) string {
	successURL := h.cfg.GitHubOAuthSuccessRedirectURL
	if successURL == "" && h.cfg.FrontendBaseURL != "" {
		successURL = strings.TrimSuffix(h.cfg.FrontendBaseURL, "/")
	}
	if successURL == "" {
		return ""
	}

	separator := "?"
	if strings.Contains(successURL, "?") {
		separator = "&"
	}
	return fmt.Sprintf(
		"%s%skyc=%s&session_id=%s",
		successURL,
		separator,
		url.QueryEscape(kycStatus),
		url.QueryEscape(sessionID),
	)
}

func diditDecisionData(decision didit.SessionDecisionResponse) map[string]interface{} {
	combinedData := make(map[string]interface{})
	if decision.Decision != nil {
		combinedData["decision"] = decision.Decision
	}
	if decision.Data != nil {
		combinedData["data"] = decision.Data
	}
	for k, v := range decision.ExtraFields {
		combinedData[k] = v
	}
	return combinedData
}

// verifyDiditSignature validates Didit's HMAC-SHA256 signature over the raw body.
func verifyDiditSignature(secret string, body []byte, signatureHeader string, timestampHeader string) bool {
	if strings.TrimSpace(secret) == "" || strings.TrimSpace(signatureHeader) == "" {
		return false
	}
	if !diditTimestampFresh(timestampHeader) {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	wantHex := hex.EncodeToString(mac.Sum(nil))

	gotHex := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(signatureHeader), "sha256="))

	return subtle.ConstantTimeCompare([]byte(gotHex), []byte(wantHex)) == 1
}

func diditTimestampFresh(timestampHeader string) bool {
	if strings.TrimSpace(timestampHeader) == "" {
		return false
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		return false
	}

	now := time.Now().Unix()
	maxSkew := int64(diditWebhookMaxTimestampSkew.Seconds())
	return timestamp >= now-maxSkew && timestamp <= now+maxSkew
}
