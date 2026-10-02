package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net/url"
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
	// The same Telegram sink support requests use. KYC content is DM-only
	// there, so this inherits that rule rather than inventing a second one.
	reviewSink SupportSink
	cfg        config.Config
	db         *db.DB
	didit      *didit.Client
	notify     *notifications.Service
}

func NewDiditWebhookHandler(cfg config.Config, d *db.DB, notify *notifications.Service) *DiditWebhookHandler {
	var diditClient *didit.Client
	if cfg.DiditAPIKey != "" {
		diditClient = didit.NewClient(cfg.DiditAPIKey)
	}
	return &DiditWebhookHandler{
		cfg:        cfg,
		db:         d,
		didit:      diditClient,
		reviewSink: newTelegramSupportSink(telegramSinkConfigFrom(cfg)),
		notify:     notify,
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

		// Handle GET request (callback redirect from Didit)
		if c.Method() == "GET" {
			// The callback also carries a `status` query param. It is
			// deliberately not read. This request is a browser redirect, so
			// its query string is whatever the person holding the link typed:
			// "status=Approved" here once marked somebody verified whenever
			// Didit's API was slow or unconfigured, and verification is what
			// unlocks GrainHack and Founding Pool payouts. The session id is
			// only used to ask Didit what the decision actually is.
			sessionID = c.Query("verificationSessionId")

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
			// event.Status is signed by Didit, so unlike the GET query it is
			// not forgeable - but it is still not applied. One source of truth
			// for both paths: the decision read back from Didit's API. A
			// signed body status used only when that read fails would mean
			// the rule is "Didit decides, unless Didit is down", and the
			// whole point is that an outage leaves KYC exactly as it was.
			sessionID = event.SessionID
		}

		if sessionID == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_session_id"})
		}

		// Find user by session ID. The stored status comes back too so that a
		// request we decline to act on can still say what we hold.
		var userID uuid.UUID
		var storedStatus string
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT id, COALESCE(kyc_status, '')
FROM users
WHERE kyc_session_id = $1
`, sessionID).Scan(&userID, &storedStatus)
		if err != nil {
			// Session not found - might be from another system or invalid
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session_not_found"})
		}

		// The decision fetched from Didit's API is the only thing that may
		// change kyc_status here. There used to be a fallback to the
		// query/body status when this fetch failed or no client was
		// configured; that fallback is what made the GET callback fail open.
		//
		// When Didit cannot be asked, nothing changes. Nothing is lost by
		// waiting: the /auth/kyc/status poll and KYCStatusReconciler both
		// re-read the same decision later, and a POST answered 5xx is
		// redelivered by Didit. Only the session id is logged - the error
		// from the client can carry a response body.
		if h.didit == nil {
			slog.Warn("didit webhook: no Didit client configured, kyc_status left unchanged",
				"session_id", sessionID)
			return h.respondUnchanged(c, sessionID, storedStatus, "didit_not_configured")
		}
		decision, err := h.didit.GetSessionDecision(c.Context(), sessionID)
		if err != nil {
			slog.Warn("didit webhook: could not fetch decision from Didit, kyc_status left unchanged",
				"session_id", sessionID)
			return h.respondUnchanged(c, sessionID, storedStatus, "didit_unreachable")
		}

		kycStatus, recognised := mapDiditStatus(decision.Status)
		// Store both Decision and Data from Didit response
		decisionData := map[string]interface{}{
			"decision": decision.Decision,
			"data":     decision.Data,
		}

		// An unrecognised status must not overwrite a real one. Ack the
		// delivery so Didit does not retry a payload we will never understand,
		// but leave the row alone - the stored status stays whatever the last
		// recognised decision made it, and the next poll re-reads the truth.
		// mapDiditStatus has already logged the unknown value at error level.
		if !recognised {
			slog.Warn("didit webhook: unrecognised status, kyc_status left unchanged",
				"session_id", sessionID, "didit_status", decision.Status)
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"ok": true, "ignored": "unrecognised_status",
			})
		}

		// Store decision data as JSONB (includes both Decision and Data)
		decisionJSON, _ := json.Marshal(decisionData)

		// One shared statement for both sync paths - see applyKYCStatus.
		//
		// This used to read the previous status in a separate SELECT and said
		// so itself: "a separate statement and therefore racy". That was
		// tolerable while the value only guarded referral completion. It is
		// not tolerable now that a contributor-facing notification hangs off
		// it, because a stale previous value either misses the message or
		// sends it twice when Didit redelivers.
		previousStatus, changed, err := applyKYCStatus(c.Context(), h.db, userID, kycStatus, decisionJSON)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "kyc_update_failed"})
		}

		// Told on every transition, in both directions. Somebody who was
		// verified and no longer is must not find out by noticing, and a
		// redelivery of a status they already have must not tell them twice -
		// which is why this is gated on `changed` rather than on the write.
		if changed {
			notifyKYCStatusChange(c.Context(), h.notify, userID, previousStatus, kycStatus)
		}

		if kycStatus == "verified" && previousStatus != "verified" {
			maybeCompleteReferral(c.Context(), h.db, h.notify, userID)
		}

		// A session entering review is waiting on US - Didit routes it to the
		// integrator's queue, not their own. Alerting is deduped on session id
		// rather than gated on the previous status: a redelivery must not
		// produce a second message, and an alert missed because the first
		// delivery was dropped must still be sendable when a later one
		// arrives. Didit retries twice and then gives up.
		if kycStatus == "in_review" {
			alertAdminOfKYCReview(c.Context(), h.db, h.reviewSink, userID, sessionID, "webhook")
		}

		// For GET requests (callback redirect), send the browser back to the app
		if c.Method() == "GET" {
			if redirected, err := h.redirectToApp(c, sessionID, kycStatus); redirected {
				return err
			}
		}

		// For POST requests (webhook), return JSON
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "status": kycStatus})
	}
}

// respondUnchanged answers a request that was authenticated (or, for GET, at
// least named a real session) but could not be checked against Didit, so
// kyc_status was left alone.
//
// GET is a person's browser coming back from Didit, so it gets the same
// redirect a successful callback gets - the app's billing tab polls
// /auth/kyc/status for the real answer and never read anything from this
// response. POST is Didit's webhook delivery, so it gets a 5xx: Didit retries
// a failed delivery, and a retry a few minutes later is the cheapest chance of
// applying the decision once the API answers again.
func (h *DiditWebhookHandler) respondUnchanged(c *fiber.Ctx, sessionID, storedStatus, reason string) error {
	if c.Method() == "GET" {
		if redirected, err := h.redirectToApp(c, sessionID, storedStatus); redirected {
			return err
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok": true, "status": storedStatus, "ignored": reason,
		})
	}
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": reason})
}

// redirectToApp sends the callback's browser back to the frontend. It reports
// false when no frontend URL is configured so the caller can answer in JSON.
//
// `kyc` carries the status we actually hold. It used to say "verified" on
// every callback, including declined ones and ones we never checked. Nothing
// in the frontend reads it, but a URL that claims a verification that did not
// happen is the same mistake as the one above, made in the other direction.
func (h *DiditWebhookHandler) redirectToApp(c *fiber.Ctx, sessionID, status string) (bool, error) {
	successURL := h.cfg.GitHubOAuthSuccessRedirectURL
	if successURL == "" && h.cfg.FrontendBaseURL != "" {
		successURL = strings.TrimSuffix(h.cfg.FrontendBaseURL, "/")
	}
	if successURL == "" {
		return false, nil
	}
	q := url.Values{}
	q.Set("kyc", status)
	q.Set("session_id", sessionID)
	return true, c.Redirect(successURL+"?"+q.Encode(), fiber.StatusFound)
}
