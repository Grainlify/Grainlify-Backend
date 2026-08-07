package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// diditSignatureMaxAgeSeconds bounds how old an X-Timestamp may be before a
// webhook is rejected as a replay, per Didit's documented 5-minute window.
const diditSignatureMaxAgeSeconds = 300

// verifyDiditSignature checks the HMAC-SHA256 X-Signature header (computed
// by Didit over the exact raw request body bytes) against the configured
// webhook secret, plus the X-Timestamp replay window. Without this, anyone
// who learns or guesses a session_id can POST a forged status update and
// have it applied directly - the session_id is looked up with no other
// authentication.
func verifyDiditSignature(secret string, body []byte, signatureHeader string, timestampHeader string) bool {
	if secret == "" || signatureHeader == "" || timestampHeader == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return false
	}
	if math.Abs(float64(time.Now().Unix()-ts)) > diditSignatureMaxAgeSeconds {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signatureHeader)))
}

type DiditWebhookHandler struct {
	cfg    config.Config
	db     *db.DB
	didit  *didit.Client
	notify *notifications.Service
}

func NewDiditWebhookHandler(cfg config.Config, d *db.DB, notify *notifications.Service) *DiditWebhookHandler {
	var diditClient *didit.Client
	if cfg.DiditAPIKey != "" {
		diditClient = didit.NewClient(cfg.DiditAPIKey)
	}
	return &DiditWebhookHandler{
		cfg:    cfg,
		db:     d,
		didit:  diditClient,
		notify: notify,
	}
}

// WebhookEvent represents a Didit webhook event
type WebhookEvent struct {
	Event     string                 `json:"event"` // e.g., "status.updated", "data.updated"
	SessionID string                 `json:"session_id"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Status    string                 `json:"status,omitempty"`
}

// Receive handles incoming Didit webhook events and callback redirects
// Supports both:
// - GET requests with query params (callback redirect from Didit)
// - POST requests with JSON body (webhook events from Didit)
func (h *DiditWebhookHandler) Receive() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		var sessionID string
		var status string

		// Handle GET request (callback redirect from Didit)
		if c.Method() == "GET" {
			sessionID = c.Query("verificationSessionId")
			status = c.Query("status")

			if sessionID == "" {
				// Try alternative query param name
				sessionID = c.Query("session_id")
			}
		} else {
			// Handle POST request (webhook event from Didit) - authenticate it
			// before trusting anything in the body.
			if h.cfg.DiditWebhookSecret == "" {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "webhook_secret_not_configured"})
			}
			if !verifyDiditSignature(h.cfg.DiditWebhookSecret, c.Body(), c.Get("X-Signature"), c.Get("X-Timestamp")) {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_signature"})
			}

			var event WebhookEvent
			if err := c.BodyParser(&event); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_json"})
			}
			sessionID = event.SessionID
			status = event.Status
		}

		if sessionID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_session_id"})
		}

		// Find user by session ID
		var userID uuid.UUID
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT id
FROM users
WHERE kyc_session_id = $1
`, sessionID).Scan(&userID)
		if err != nil {
			// Session not found - might be from another system or invalid
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session_not_found"})
		}

		// Process status update
		// Fetch latest decision from Didit API if available
		var kycStatus string
		var decisionData map[string]interface{}

		if h.didit != nil {
			decision, err := h.didit.GetSessionDecision(c.Context(), sessionID)
			if err != nil {
				// If API call fails, use status from query/body
				kycStatus = mapDiditStatus(status)
			} else {
				// Map Didit status to our KYC status
				kycStatus = mapDiditStatus(decision.Status)
				// Store both Decision and Data from Didit response
				decisionData = map[string]interface{}{
					"decision": decision.Decision,
					"data":     decision.Data,
				}
			}
		} else {
			// If no Didit client, use status from query/body
			kycStatus = mapDiditStatus(status)
		}

		// Store decision data as JSONB (includes both Decision and Data)
		decisionJSON, _ := json.Marshal(decisionData)

		// Capture the pre-update status so a duplicate "verified" delivery
		// (Didit may redeliver webhooks) doesn't re-trigger referral
		// completion - only a genuine not-verified -> verified transition
		// should.
		var previousStatus string
		_ = h.db.Pool.QueryRow(c.Context(), `SELECT COALESCE(kyc_status, '') FROM users WHERE id = $1`, userID).Scan(&previousStatus)

		// Update user KYC status
		_, err = h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_status = $1,
    kyc_data = $2,
    kyc_verified_at = CASE WHEN $1 = 'verified' THEN now() ELSE kyc_verified_at END,
    updated_at = now()
WHERE id = $3
`, kycStatus, decisionJSON, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "kyc_update_failed"})
		}

		if kycStatus == "verified" && previousStatus != "verified" {
			maybeCompleteReferral(c.Context(), h.db, h.notify, userID)
		}

		// For GET requests (callback redirect), redirect to success page
		if c.Method() == "GET" {
			// Redirect to frontend with success message
			successURL := h.cfg.GitHubOAuthSuccessRedirectURL
			if successURL == "" && h.cfg.FrontendBaseURL != "" {
				successURL = strings.TrimSuffix(h.cfg.FrontendBaseURL, "/")
			}
			if successURL != "" {
				// Add query params to indicate success
				redirectURL := fmt.Sprintf("%s?kyc=verified&session_id=%s", successURL, sessionID)
				return c.Redirect(redirectURL, fiber.StatusFound)
			}
		}

		// For POST requests (webhook), return JSON
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "status": kycStatus})
	}
}
