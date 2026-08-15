package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// extractKYCInfo extracts structured information from Didit response data
func extractKYCInfo(data map[string]interface{}) map[string]interface{} {
	extracted := make(map[string]interface{})

	// Extract personal information from id_verification
	if idVerification, ok := data["id_verification"].(map[string]interface{}); ok {
		if firstName, ok := idVerification["first_name"].(string); ok && firstName != "" {
			extracted["first_name"] = firstName
		}
		if lastName, ok := idVerification["last_name"].(string); ok && lastName != "" {
			extracted["last_name"] = lastName
		}
		if fullName, ok := idVerification["full_name"].(string); ok && fullName != "" {
			extracted["full_name"] = fullName
		}
		if address, ok := idVerification["address"].(string); ok && address != "" {
			extracted["address"] = address
		}
		if dob, ok := idVerification["date_of_birth"].(string); ok && dob != "" {
			extracted["date_of_birth"] = dob
		}
		if age, ok := idVerification["age"].(float64); ok {
			extracted["age"] = int(age)
		}
		if documentType, ok := idVerification["document_type"].(string); ok && documentType != "" {
			extracted["document_type"] = documentType
		}
		if documentNumber, ok := idVerification["document_number"].(string); ok && documentNumber != "" {
			extracted["document_number"] = documentNumber
		}
		if status, ok := idVerification["status"].(string); ok && status != "" {
			extracted["id_verification_status"] = status
		}
	}

	// Extract face match information
	if faceMatch, ok := data["face_match"].(map[string]interface{}); ok {
		if score, ok := faceMatch["score"].(float64); ok {
			extracted["face_match_score"] = score
		}
		if status, ok := faceMatch["status"].(string); ok && status != "" {
			extracted["face_match_status"] = status
		}
	}

	return extracted
}

// diditSessionUnreachable reports whether an error from Didit means the stored
// session can never produce a decision, so the contributor should be released
// to start a new one.
//
// **403 counts.** A session that belongs to a retired Didit account answers
// "You do not have permission to perform this action" under the new API key,
// and is as gone as a deleted one - no decision will ever arrive for it. Until
// this included 403, the difference between two error strings was the
// difference between self-healing and stuck-until-an-admin-notices: three
// contributors sat in in_review with sessions on the old account after the key
// migration, and only came unstuck because somebody went looking.
//
// This is one function because it was previously two lists, at the two call
// sites below, and they had already drifted - Status() matched "does not
// exist", "no such" and "not available" while Start() did not, so the same
// dead session could release a contributor on one endpoint and block them on
// the other. A rule expressed twice is a rule that disagrees with itself.
//
// Deliberately substring matching on the message rather than on a status code:
// didit.Client returns a formatted error string, not a typed one, and
// widening that interface is a larger change than this needs. The strings are
// broad on purpose - a false positive costs a contributor an extra click to
// start again, a false negative strands them.
func diditSessionUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		// Gone.
		"404", "not found", "not_found", "does not exist", "doesn't exist",
		"no such", "not available", "deleted", "invalid",
		// Not ours: belongs to another (retired) Didit account.
		"403", "permission", "forbidden", "unauthorized", "401",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// mapDiditStatus maps a Didit status onto ours, reporting whether it
// recognised the input.
//
// **The bool is the point.** This used to return "not_started" for anything it
// did not recognise, which is the worst available default: not_started is one
// of the three states canStartNewKYCSession treats as "free to begin", so an
// unrecognised status silently rewrote a live verification into "never
// started". A contributor midway through would be recorded as not having
// begun, and any status carrying a decision we could not parse would discard
// that decision.
//
// It mattered because the set was incomplete. Didit's v3 webhook sends
// "Approved" | "Declined" | "In Review" | "In Progress" | "Not Started" |
// "Abandoned" | "Expired" | "Kyc Expired" | "Resubmitted" | "Awaiting User",
// and five of those ten fell through: "In Progress" (this switch had
// "in_progress" and "inprogress" but not the spaced form Didit actually
// sends), "Abandoned", "Kyc Expired", "Resubmitted" and "Awaiting User".
//
// Callers must not write the status when ok is false. Keeping the row as it is
// loses nothing - the next poll or webhook re-reads the authoritative decision
// from Didit - whereas writing a guess destroys state we cannot recover.
//
// Status flow: not_started -> pending -> in_review -> verified/rejected/expired
func mapDiditStatus(diditStatus string) (string, bool) {
	status := strings.ToLower(strings.TrimSpace(diditStatus))
	switch status {
	case "approved", "verified":
		return "verified", true
	case "rejected", "declined":
		return "rejected", true
	case "in review", "inreview":
		// Didit is actively reviewing the verification.
		return "in_review", true
	case "resubmitted":
		// The contributor supplied fresh documents after a request for more.
		// That goes back to Didit for a decision, so it is a review state, not
		// a "start again" one.
		return "in_review", true
	case "pending", "in progress", "in_progress", "inprogress":
		// Started, not yet decided. "in progress" with a space is the form v3
		// actually sends; its absence here is what made this the most common
		// unrecognised value.
		return "pending", true
	case "awaiting user":
		// Didit is waiting on the contributor to finish something. The session
		// is live, so this is pending rather than expired: they should resume
		// the existing link, which /kyc/status hands back, rather than start a
		// second session.
		return "pending", true
	case "expired", "kyc expired":
		return "expired", true
	case "abandoned":
		// Started and walked away. Deliberately mapped to expired rather than
		// to a blocking state: expired is in canStartNewKYCSession's allow
		// list, so an abandoned attempt lets them begin again instead of
		// stranding them behind a session they will never finish.
		return "expired", true
	case "not started", "notstarted", "not_started":
		// A session exists but the link was never opened. Distinct from
		// pending: verification has not begun.
		return "not_started", true
	default:
		// Unrecognised. The caller keeps the existing status rather than
		// writing a guess. Logged at error level because a status we have
		// never seen means Didit has added one and this switch needs a
		// deliberate decision about it - silence here is how the previous
		// version turned five real statuses into "never started".
		slog.Error("unrecognised didit status - leaving stored kyc_status unchanged",
			"status", diditStatus)
		return "", false
	}
}

type KYCHandler struct {
	cfg    config.Config
	db     *db.DB
	didit  *didit.Client
	notify *notifications.Service
}

func NewKYCHandler(cfg config.Config, d *db.DB, notify *notifications.Service) *KYCHandler {
	var diditClient *didit.Client
	if cfg.DiditAPIKey != "" {
		diditClient = didit.NewClient(cfg.DiditAPIKey)
	}
	return &KYCHandler{
		cfg:    cfg,
		db:     d,
		didit:  diditClient,
		notify: notify,
	}
}

// Start initiates a KYC verification session for the authenticated user
// canStartNewKYCSession reports whether a user in this state may open a new
// verification session.
//
// Extracted from Start() so the rule can be tested directly: the handler needs
// a configured Didit client to reach this point, and the test suite
// deliberately never contacts Didit.
//
//	nil          no session has ever been created
//	expired      the session was deleted in the Didit dashboard
//	not_started  a session exists but the user never opened the link
//
// The third is the one that changed. It reads as "in progress" and is the
// opposite: the link was created and never followed, so there is no in-flight
// verification to interrupt and no result to lose. Treating it as active left
// users permanently unable to start - the only exit was an admin deleting the
// session by hand - and it is the state you land in by clicking "verify" once
// and closing the tab.
//
// pending, in_review, verified and rejected all represent real progress and
// are still protected.
func canStartNewKYCSession(status *string) bool {
	if status == nil {
		return true
	}
	switch *status {
	case "", "expired", "not_started":
		return true
	default:
		return false
	}
}

func (h *KYCHandler) Start() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if h.didit == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "kyc_not_configured", "message": "DIDIT_API_KEY and DIDIT_WORKFLOW_ID must be set"})
		}
		if h.cfg.DiditWorkflowID == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "kyc_not_configured", "message": "DIDIT_WORKFLOW_ID must be set"})
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		// Check if user already has an active KYC session
		var existingSessionID *string
		var existingStatus *string
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT kyc_session_id, kyc_status
FROM users
WHERE id = $1
`, userID).Scan(&existingSessionID, &existingStatus)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "user_lookup_failed"})
		}

		// A new session is allowed when there is nothing worth protecting:
		//
		//   NULL         no session has ever been created
		//   expired      the session was deleted in the Didit dashboard
		//   not_started  a session exists but the user never opened the link
		//
		// "not_started" used to be treated as an active session and blocked. It
		// is the opposite: it means the link was created and never followed, so
		// there is no in-flight verification to interrupt and no result to lose.
		// Blocking it left users permanently unable to start - the only way out
		// was an admin deleting the session by hand - and it is the state a user
		// lands in simply by clicking "verify" once and closing the tab.
		//
		// Anything else (pending, in_review, verified, rejected) represents real
		// progress and is still protected.
		if existingSessionID != nil && !canStartNewKYCSession(existingStatus) {
			// Get stored KYC data to find session URL
			var kycDataBytes []byte
			_ = h.db.Pool.QueryRow(c.Context(), `
SELECT kyc_data
FROM users
WHERE id = $1
`, userID).Scan(&kycDataBytes)

			var sessionURL string
			if len(kycDataBytes) > 0 {
				var kycDataMap map[string]interface{}
				if err := json.Unmarshal(kycDataBytes, &kycDataMap); err == nil {
					if url, ok := kycDataMap["session_url"].(string); ok && url != "" {
						sessionURL = url
					}
				}
			}

			// If no URL in stored data, construct it from session_id
			if sessionURL == "" && *existingSessionID != "" {
				// Construct URL: https://verify.didit.me/session/{short_id}
				// The session_id is UUID, but Didit uses a short ID in the URL
				// We'll try to get it from Didit API or construct a placeholder
				sessionURL = fmt.Sprintf("https://verify.didit.me/session/%s", *existingSessionID)
			}

			// Check if the existing session still exists in Didit
			// If it doesn't exist (404), it means admin deleted it - mark as expired and allow new session
			if h.didit != nil {
				decision, err := h.didit.GetSessionDecision(c.Context(), *existingSessionID)
				if err != nil {
					// One shared definition of "this session can never answer" -
					// see diditSessionUnreachable. Includes 403, which is what a
					// session from a retired Didit account returns.
					if diditSessionUnreachable(err) {
						// Session was deleted in Didit dashboard - mark as expired and allow new session
						_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_status = 'expired',
    kyc_session_id = NULL,
    updated_at = now()
WHERE id = $1
`, userID)
						slog.Info("session deleted in didit dashboard, marked as expired", "session_id", *existingSessionID, "user_id", userID)
						// Continue to create new session
					} else {
						// Session exists in Didit - don't allow new session, but return URL if we have it
						response := fiber.Map{
							"error":      "kyc_session_exists",
							"message":    fmt.Sprintf("You already have a KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
							"session_id": *existingSessionID,
							"status":     *existingStatus,
						}
						if sessionURL != "" {
							response["url"] = sessionURL
						}
						return c.Status(fiber.StatusConflict).JSON(response)
					}
				} else {
					// Session exists in Didit - extract session_url from response if available
					if decision.ExtraFields != nil {
						if url, ok := decision.ExtraFields["session_url"].(string); ok && url != "" {
							sessionURL = url
						}
					}
					// Don't allow new session
					response := fiber.Map{
						"error":      "kyc_session_exists",
						"message":    fmt.Sprintf("You already have an active KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
						"session_id": *existingSessionID,
						"status":     *existingStatus,
					}
					if sessionURL != "" {
						response["url"] = sessionURL
					}
					return c.Status(fiber.StatusConflict).JSON(response)
				}
			} else {
				// No Didit client - check status directly. Same rule as above:
				// expired means the session is gone, and not_started is filtered
				// out before we get here.
				if *existingStatus != "expired" {
					response := fiber.Map{
						"error":      "kyc_session_exists",
						"message":    fmt.Sprintf("You already have a KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
						"session_id": *existingSessionID,
						"status":     *existingStatus,
					}
					if sessionURL != "" {
						response["url"] = sessionURL
					}
					return c.Status(fiber.StatusConflict).JSON(response)
				}
			}
		}

		// Build callback URL if public base URL is configured
		// Must be a full URL with protocol (https://)
		var callbackURL string
		if h.cfg.PublicBaseURL != "" {
			baseURL := strings.TrimRight(h.cfg.PublicBaseURL, "/")
			// Ensure it has a protocol
			if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
				baseURL = "https://" + baseURL
			}
			callbackURL = fmt.Sprintf("%s/webhooks/didit", baseURL)
		}

		// Create Didit session
		slog.Info("creating didit session", "user_id", userID, "workflow_id", h.cfg.DiditWorkflowID, "callback", callbackURL)
		sessionResp, err := h.didit.CreateSession(c.Context(), didit.CreateSessionRequest{
			WorkflowID: h.cfg.DiditWorkflowID,
			VendorData: userID.String(),
			Callback:   callbackURL,
		})
		if err != nil {
			slog.Error("didit create session failed", "error", err, "user_id", userID, "workflow_id", h.cfg.DiditWorkflowID)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   "kyc_session_create_failed",
				"message": err.Error(),
			})
		}
		slog.Info("didit session created", "session_id", sessionResp.SessionID, "url", sessionResp.URL, "user_id", userID)

		// Store session ID and URL in database (replaces any existing session)
		// Store the URL in kyc_data so we can retrieve it later
		// Initial status should be 'not_started' since user hasn't clicked the link yet
		// The Status() endpoint will update it to 'pending' when user actually starts verification
		sessionDataJSON, _ := json.Marshal(map[string]interface{}{
			"session_url": sessionResp.URL,
		})

		slog.Info("storing kyc session in database", "user_id", userID, "session_id", sessionResp.SessionID, "status", "not_started")
		result, err := h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_session_id = $1,
    kyc_status = 'not_started',
    kyc_data = $2,
    updated_at = now()
WHERE id = $3
`, sessionResp.SessionID, sessionDataJSON, userID)
		if err != nil {
			slog.Error("failed to store kyc session in database",
				"error", err,
				"user_id", userID,
				"session_id", sessionResp.SessionID,
				"kyc_data_size", len(sessionDataJSON),
				"error_type", fmt.Sprintf("%T", err))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   "kyc_session_store_failed",
				"message": err.Error(),
			})
		}

		rowsAffected := result.RowsAffected()
		slog.Info("stored new kyc session", "user_id", userID, "session_id", sessionResp.SessionID, "rows_affected", rowsAffected)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"session_id": sessionResp.SessionID,
			"url":        sessionResp.URL,
		})
	}
}

// Status returns the current KYC verification status for the authenticated user
// If status is pending and we have a session_id, fetches latest status from Didit API
func (h *KYCHandler) Status() fiber.Handler {
	return func(c *fiber.Ctx) error {
		slog.Info("kyc status request started", "path", c.Path(), "method", c.Method())

		if h.db == nil || h.db.Pool == nil {
			slog.Error("db not configured in kyc status handler")
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		if sub == "" {
			slog.Error("no user id in context")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		userID, err := uuid.Parse(sub)
		if err != nil {
			slog.Error("failed to parse user id", "sub", sub, "error", err)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		slog.Info("fetching kyc status from database", "user_id", userID)

		var kycStatus *string
		var kycSessionID *string
		var kycVerifiedAt *time.Time
		var kycData []byte

		err = h.db.Pool.QueryRow(c.Context(), `
SELECT kyc_status, kyc_session_id, kyc_verified_at, kyc_data
FROM users
WHERE id = $1
`, userID).Scan(&kycStatus, &kycSessionID, &kycVerifiedAt, &kycData)
		if err != nil {
			slog.Error("failed to fetch kyc status from database", "user_id", userID, "error", err, "error_type", fmt.Sprintf("%T", err))
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error":   "kyc_status_fetch_failed",
				"message": err.Error(),
			})
		}

		// Log actual values, not pointers
		statusStr := "nil"
		if kycStatus != nil {
			statusStr = *kycStatus
		}
		sessionIDStr := "nil"
		if kycSessionID != nil {
			sessionIDStr = *kycSessionID
		}
		verifiedAtLogStr := "nil"
		if kycVerifiedAt != nil {
			verifiedAtLogStr = kycVerifiedAt.Format(time.RFC3339)
		}

		slog.Info("fetched kyc status from database",
			"user_id", userID,
			"kyc_status", statusStr,
			"kyc_session_id", sessionIDStr,
			"kyc_verified_at", verifiedAtLogStr,
			"kyc_data_size", len(kycData))

		// If we have a session ID, always fetch latest status from Didit API
		// This ensures we detect if the session was deleted in Didit dashboard
		// and get accurate status updates (including not_started -> pending transitions)
		if kycSessionID != nil && *kycSessionID != "" && h.didit != nil {
			currentStatusStr := "nil"
			if kycStatus != nil {
				currentStatusStr = *kycStatus
			}
			slog.Info("checking session with didit api", "session_id", *kycSessionID, "current_status", currentStatusStr)
			// Always fetch to check if session still exists (especially for pending status)
			decision, err := h.didit.GetSessionDecision(c.Context(), *kycSessionID)
			if err != nil {
				// If API call fails, check if it's because session was deleted
				currentStatusStr := "nil"
				if kycStatus != nil {
					currentStatusStr = *kycStatus
				}
				slog.Warn("didit api call failed",
					"session_id", *kycSessionID,
					"error", err.Error(),
					"current_status", currentStatusStr,
					"error_type", fmt.Sprintf("%T", err))

				// Check if error indicates session not found, deleted, or invalid
				// Check for various error patterns that indicate session doesn't exist
				// The error format from Didit client is: "didit get decision failed: status 404, error: ..., body: ..."
				// Same definition as Start() uses. These were two separate
				// lists that had already drifted apart.
				isDeleted := diditSessionUnreachable(err)

				if isDeleted {
					previousStatusStr := "nil"
					if kycStatus != nil {
						previousStatusStr = *kycStatus
					}
					slog.Info("session deleted in didit - marking as expired",
						"session_id", *kycSessionID,
						"user_id", userID,
						"previous_status", previousStatusStr)
					// Session was deleted in Didit dashboard - mark as expired
					expiredStatus := "expired"
					// Store the session ID before clearing it for logging
					deletedSessionID := *kycSessionID
					_, updateErr := h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_status = $1,
    kyc_session_id = NULL,
    updated_at = now()
WHERE id = $2
`, expiredStatus, userID)
					if updateErr != nil {
						slog.Error("failed to mark session as expired in database",
							"error", updateErr,
							"user_id", userID,
							"session_id", deletedSessionID,
							"error_type", fmt.Sprintf("%T", updateErr))
						// Don't return error - continue with existing status
					} else {
						kycStatus = &expiredStatus
						kycSessionID = nil // Clear session ID since it's invalid
						previousStatusStr := "nil"
						if kycStatus != nil {
							previousStatusStr = *kycStatus
						}
						slog.Info("marked session as expired - deleted in didit dashboard",
							"session_id", deletedSessionID,
							"user_id", userID,
							"previous_status", previousStatusStr,
							"new_status", expiredStatus)
					}
				} else {
					// For other errors (network, timeout, etc.), log but keep existing status
					currentStatusStr := "nil"
					if kycStatus != nil {
						currentStatusStr = *kycStatus
					}
					slog.Warn("didit api error but session may still exist",
						"session_id", *kycSessionID,
						"error", err.Error(),
						"current_status", currentStatusStr)
				}
			} else if newStatus, ok := mapDiditStatus(decision.Status); !ok {
				// Unrecognised status: keep whatever is stored. Writing a guess
				// here is how a live verification became "never started".
				// mapDiditStatus has already logged the value.
				slog.Warn("kyc status poll: leaving stored status unchanged",
					"session_id", *kycSessionID, "didit_status", decision.Status)
			} else {
				// Session exists in Didit - update status based on Didit response

				// Log the full decision structure for debugging
				decisionJSONDebug, _ := json.Marshal(decision.Decision)
				dataJSONDebug, _ := json.Marshal(decision.Data)
				extraFieldsJSON, _ := json.Marshal(decision.ExtraFields)
				currentStatusStr := "nil"
				if kycStatus != nil {
					currentStatusStr = *kycStatus
				}
				slog.Info("fetched didit status",
					"session_id", *kycSessionID,
					"didit_status", decision.Status,
					"mapped_status", newStatus,
					"current_db_status", currentStatusStr,
					"decision", string(decisionJSONDebug),
					"data", string(dataJSONDebug),
					"extra_fields", string(extraFieldsJSON))

				// Store Decision, Data, and any extra fields from Didit response
				combinedData := map[string]interface{}{
					"decision": decision.Decision,
					"data":     decision.Data,
				}
				// Include any extra fields (like session_url)
				for k, v := range decision.ExtraFields {
					combinedData[k] = v
				}

				// Extract structured information from the response
				extractedInfo := extractKYCInfo(combinedData)
				if len(extractedInfo) > 0 {
					combinedData["extracted"] = extractedInfo
				}

				decisionJSON, _ := json.Marshal(combinedData)

				// Update database if status changed (including not_started -> pending transitions)
				// Always update to ensure accurate status representation
				statusChanged := kycStatus == nil || *kycStatus != newStatus
				if statusChanged || *kycStatus == "rejected" {
					oldStatusStr := "nil"
					if kycStatus != nil {
						oldStatusStr = *kycStatus
					}
					_, updateErr := h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_status = $1,
    kyc_data = $2,
    kyc_verified_at = CASE WHEN $1 = 'verified' THEN now() ELSE kyc_verified_at END,
    updated_at = now()
WHERE id = $3
`, newStatus, decisionJSON, userID)
					if updateErr != nil {
						slog.Error("failed to update kyc status", "error", updateErr, "user_id", userID, "old_status", oldStatusStr, "new_status", newStatus)
					} else {
						kycStatus = &newStatus
						// Update kycData with latest decision data
						kycData = decisionJSON
						if statusChanged {
							slog.Info("kyc status changed", "user_id", userID, "old_status", oldStatusStr, "new_status", newStatus, "didit_status", decision.Status)
							if newStatus == "verified" {
								maybeCompleteReferral(c.Context(), h.db, h.notify, userID)
							}
						}
					}
				} else {
					// Status hasn't changed, but still update kyc_data if we have new info
					_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_data = $1,
    updated_at = now()
WHERE id = $2
`, decisionJSON, userID)
					kycData = decisionJSON
				}
			}
		}

		var kycDataMap map[string]interface{}
		if len(kycData) > 0 {
			_ = json.Unmarshal(kycData, &kycDataMap)
		}

		// Extract rejection reasons and get extracted info
		var extractedInfo map[string]interface{}
		var rejectionReason interface{}

		if kycDataMap != nil {
			// Get extracted info if it exists, otherwise extract it now
			if extracted, ok := kycDataMap["extracted"].(map[string]interface{}); ok {
				extractedInfo = extracted
			} else {
				// Extract info if not already extracted
				extractedInfo = extractKYCInfo(kycDataMap)
				if len(extractedInfo) > 0 {
					// Store extracted info
					mergedData := make(map[string]interface{})
					if len(kycData) > 0 {
						_ = json.Unmarshal(kycData, &mergedData)
					}
					mergedData["extracted"] = extractedInfo
					mergedJSON, _ := json.Marshal(mergedData)

					_, _ = h.db.Pool.Exec(c.Context(), `
UPDATE users
SET kyc_data = $1,
    updated_at = now()
WHERE id = $2
`, mergedJSON, userID)
				}
			}

			// Extract rejection reasons from warnings
			var rejectionReasons []string

			// Check face_match warnings
			if faceMatch, ok := kycDataMap["face_match"].(map[string]interface{}); ok {
				if warnings, ok := faceMatch["warnings"].([]interface{}); ok {
					for _, warning := range warnings {
						if w, ok := warning.(map[string]interface{}); ok {
							if longDesc, ok := w["long_description"].(string); ok && longDesc != "" {
								rejectionReasons = append(rejectionReasons, longDesc)
							} else if shortDesc, ok := w["short_description"].(string); ok && shortDesc != "" {
								rejectionReasons = append(rejectionReasons, shortDesc)
							}
						}
					}
				}
			}

			// Check other feature warnings (id_verification, liveness, etc.)
			featuresToCheck := []string{"id_verification", "liveness", "ip_analysis"}
			for _, featureName := range featuresToCheck {
				if feature, ok := kycDataMap[featureName].(map[string]interface{}); ok {
					if warnings, ok := feature["warnings"].([]interface{}); ok {
						for _, warning := range warnings {
							if w, ok := warning.(map[string]interface{}); ok {
								if longDesc, ok := w["long_description"].(string); ok && longDesc != "" {
									rejectionReasons = append(rejectionReasons, longDesc)
								} else if shortDesc, ok := w["short_description"].(string); ok && shortDesc != "" {
									rejectionReasons = append(rejectionReasons, shortDesc)
								}
							}
						}
					}
				}
			}

			// If rejected, set rejection reason
			if kycStatus != nil && *kycStatus == "rejected" {
				if len(rejectionReasons) > 0 {
					rejectionReason = strings.Join(rejectionReasons, "; ")
					if extractedInfo == nil {
						extractedInfo = make(map[string]interface{})
					}
					extractedInfo["rejection_reasons"] = rejectionReasons
				} else {
					// Fallback: check for any status fields that indicate rejection
					rejectionReason = "Verification declined"
				}
			}
		}

		// Format verified_at as ISO8601 string for JSON response
		var verifiedAtStr *string
		if kycVerifiedAt != nil {
			formatted := kycVerifiedAt.Format(time.RFC3339)
			verifiedAtStr = &formatted
		}

		response := fiber.Map{
			"status":      kycStatus,
			"session_id":  kycSessionID,
			"verified_at": verifiedAtStr,
			"data":        kycDataMap,
		}

		// Add extracted information if available
		if extractedInfo != nil && len(extractedInfo) > 0 {
			response["extracted"] = extractedInfo
		}

		// Add rejection reason if available
		if rejectionReason != nil {
			response["rejection_reason"] = rejectionReason
		}

		// Log actual status values for debugging
		responseStatusStr := "nil"
		if kycStatus != nil {
			responseStatusStr = *kycStatus
		}
		responseSessionIDStr := "nil"
		if kycSessionID != nil {
			responseSessionIDStr = *kycSessionID
		}
		responseVerifiedAtLogStr := "nil"
		if verifiedAtStr != nil {
			responseVerifiedAtLogStr = *verifiedAtStr
		}

		slog.Info("returning kyc status response",
			"user_id", userID,
			"status", responseStatusStr,
			"session_id", responseSessionIDStr,
			"verified_at", responseVerifiedAtLogStr,
			"has_extracted", extractedInfo != nil && len(extractedInfo) > 0,
			"has_rejection_reason", rejectionReason != nil)

		return c.Status(fiber.StatusOK).JSON(response)
	}
}
