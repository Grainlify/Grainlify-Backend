package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
	"github.com/jagadeesh/grainlify/backend/internal/httpx"
	"github.com/jagadeesh/grainlify/backend/internal/logger"
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

// mapDiditStatus maps Didit status to our internal KYC status
// Production-ready mapping that preserves accurate status representation
// Status flow: not_started -> pending -> in_review -> verified/rejected/expired
func mapDiditStatus(diditStatus string) string {
	status := strings.ToLower(strings.TrimSpace(diditStatus))
	switch status {
	case "approved", "verified":
		return "verified"
	case "rejected", "declined":
		return "rejected"
	case "in review", "inreview":
		// Didit is actively reviewing the verification
		return "in_review"
	case "pending", "in_progress", "inprogress":
		// User has started verification process (clicked the link, submitted documents, etc.)
		// but Didit hasn't started reviewing yet
		return "pending"
	case "expired":
		return "expired"
	case "not started", "notstarted", "not_started":
		// Session exists but user hasn't clicked the verification link yet
		// This is distinct from "pending" - user hasn't begun verification
		return "not_started"
	default:
		// Unknown status - log as error for production monitoring
		slog.Error("unknown didit status - defaulting to not_started", "status", diditStatus, "original", diditStatus)
		return "not_started"
	}
}

type KYCHandler struct {
	cfg   config.Config
	db    *db.DB
	didit *didit.Client
}

func NewKYCHandler(cfg config.Config, d *db.DB) *KYCHandler {
	var diditClient *didit.Client
	if cfg.DiditAPIKey != "" {
		diditClient = didit.NewClient(cfg.DiditAPIKey)
	}
	return &KYCHandler{
		cfg:   cfg,
		db:    d,
		didit: diditClient,
	}
}

// Start initiates a KYC verification session for the authenticated user
func (h *KYCHandler) Start() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}
		if h.didit == nil {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "kyc_not_configured", "DIDIT_API_KEY and DIDIT_WORKFLOW_ID must be set")
		}
		if h.cfg.DiditWorkflowID == "" {
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "kyc_not_configured", "DIDIT_WORKFLOW_ID must be set")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		var conflictResponse fiber.Map
		var sessionIDOut string
		var sessionURLOut string

		txErr := h.db.WithTx(c.Context(), func(tx pgx.Tx) error {
			// Lock the user's row for the duration of this transaction so a
			// second concurrent request blocks here until we commit, then
			// sees the session we just created (and correctly hits the
			// "already exists" branch below instead of racing past it).
			var existingSessionID *string
			var existingStatus *string
			err := tx.QueryRow(c.Context(), `
SELECT kyc_session_id, kyc_status
FROM users
WHERE id = $1
FOR UPDATE
`, userID).Scan(&existingSessionID, &existingStatus)
			if err != nil {
				return fmt.Errorf("user_lookup_failed: %w", err)
			}

			if existingSessionID != nil && existingStatus != nil {
				var kycDataBytes []byte
				_ = tx.QueryRow(c.Context(), `
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

				if sessionURL == "" && *existingSessionID != "" {
					sessionURL = fmt.Sprintf("https://verify.didit.me/session/%s", *existingSessionID)
				}

				if h.didit != nil {
					decision, err := h.didit.GetSessionDecision(c.Context(), *existingSessionID)
					if err != nil {
						if errors.Is(err, didit.ErrSessionNotFound) {
							_, err := tx.Exec(c.Context(), `
UPDATE users
SET kyc_status = 'expired',
    kyc_session_id = NULL,
    updated_at = now()
WHERE id = $1
`, userID)
							if err != nil {
								return fmt.Errorf("mark_expired_failed: %w", err)
							}
							slog.Info("session deleted in didit dashboard, marked as expired", "session_id", *existingSessionID, "user_id", userID)
							// Fall through: continue this same transaction to create a new session below.
						} else {
							conflictResponse = fiber.Map{
								"error":      "kyc_session_exists",
								"message":    fmt.Sprintf("You already have a KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
								"session_id": *existingSessionID,
								"status":     *existingStatus,
							}
							if sessionURL != "" {
								conflictResponse["url"] = sessionURL
							}
							return nil
						}
					} else {
						if decision.ExtraFields != nil {
							if url, ok := decision.ExtraFields["session_url"].(string); ok && url != "" {
								sessionURL = url
							}
						}
						conflictResponse = fiber.Map{
							"error":      "kyc_session_exists",
							"message":    fmt.Sprintf("You already have an active KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
							"session_id": *existingSessionID,
							"status":     *existingStatus,
						}
						if sessionURL != "" {
							conflictResponse["url"] = sessionURL
						}
						return nil
					}
				} else {
					if *existingStatus != "expired" {
						conflictResponse = fiber.Map{
							"error":      "kyc_session_exists",
							"message":    fmt.Sprintf("You already have a KYC verification session (status: %s). Please complete it or contact admin to delete it.", *existingStatus),
							"session_id": *existingSessionID,
							"status":     *existingStatus,
						}
						if sessionURL != "" {
							conflictResponse["url"] = sessionURL
						}
						return nil
					}
				}
			}

			var callbackURL string
			if h.cfg.PublicBaseURL != "" {
				baseURL := strings.TrimRight(h.cfg.PublicBaseURL, "/")
				if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
					baseURL = "https://" + baseURL
				}
				callbackURL = fmt.Sprintf("%s/webhooks/didit", baseURL)
			}

			slog.Info("creating didit session", "user_id", userID, "workflow_id", h.cfg.DiditWorkflowID, "callback", callbackURL)
			sessionResp, err := h.didit.CreateSession(c.Context(), didit.CreateSessionRequest{
				WorkflowID: h.cfg.DiditWorkflowID,
				VendorData: userID.String(),
				Callback:   callbackURL,
			})
			if err != nil {
				return fmt.Errorf("kyc_session_create_failed: %w", err)
			}
			slog.Info("didit session created", "session_id", sessionResp.SessionID, "url", sessionResp.URL, "user_id", userID)

			sessionDataJSON, _ := json.Marshal(map[string]interface{}{
				"session_url": sessionResp.URL,
			})

			slog.Info("storing kyc session in database", "user_id", userID, "session_id", sessionResp.SessionID, "status", "not_started")
			_, err = tx.Exec(c.Context(), `
UPDATE users
SET kyc_session_id = $1,
    kyc_status = 'not_started',
    kyc_data = $2,
    updated_at = now()
WHERE id = $3
`, sessionResp.SessionID, sessionDataJSON, userID)
			if err != nil {
				return fmt.Errorf("kyc_session_store_failed: %w", err)
			}

			sessionIDOut = sessionResp.SessionID
			sessionURLOut = sessionResp.URL
			return nil
		})

		if txErr != nil {
			msg := txErr.Error()
			slog.Error("kyc start transaction failed", "error", logger.RedactError(txErr), "user_id", userID)
			switch {
			case strings.HasPrefix(msg, "user_lookup_failed"):
				return httpx.RespondError(c, fiber.StatusInternalServerError, "user_lookup_failed", "")
			case strings.HasPrefix(msg, "kyc_session_create_failed"):
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error":   "kyc_session_create_failed",
					"message": msg,
				})
			case strings.HasPrefix(msg, "kyc_session_store_failed"):
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error":   "kyc_session_store_failed",
					"message": msg,
				})
			default:
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error":   "kyc_start_failed",
					"message": msg,
				})
			}
		}

		if conflictResponse != nil {
			return c.Status(fiber.StatusConflict).JSON(conflictResponse)
		}

				slog.Info("stored new kyc session", "user_id", userID, "session_id", sessionIDOut)

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"session_id": sessionIDOut,
			"url":        sessionURLOut,
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
			return httpx.RespondError(c, fiber.StatusServiceUnavailable, "db_not_configured", "")
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		if sub == "" {
			slog.Error("no user id in context")
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
		}

		userID, err := uuid.Parse(sub)
		if err != nil {
			slog.Error("failed to parse user id", "sub", sub, "error", logger.RedactError(err))
			return httpx.RespondError(c, fiber.StatusUnauthorized, "invalid_user", "")
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
			slog.Error("failed to fetch kyc status from database", "user_id", userID, "error", logger.RedactError(err), "error_type", fmt.Sprintf("%T", err))
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
					"error", logger.RedactError(err),
					"current_status", currentStatusStr,
					"error_type", fmt.Sprintf("%T", err))

				// Check if error indicates session not found/deleted in Didit
				isDeleted := errors.Is(err, didit.ErrSessionNotFound)

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
							"error", logger.RedactError(updateErr),
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
						"error", logger.RedactError(err),
						"current_status", currentStatusStr)
				}
			} else {
				// Session exists in Didit - update status based on Didit response
				newStatus := mapDiditStatus(decision.Status)

				// Log the full decision structure for debugging
				redactedDecision := logger.RedactMap(decision.Decision)
				redactedData := logger.RedactMap(decision.Data)
				redactedExtraFields := logger.RedactMap(decision.ExtraFields)
				decisionJSONDebug, _ := json.Marshal(redactedDecision)
				dataJSONDebug, _ := json.Marshal(redactedData)
				extraFieldsJSON, _ := json.Marshal(redactedExtraFields)
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
						slog.Error("failed to update kyc status", "error", logger.RedactError(updateErr), "user_id", userID, "old_status", oldStatusStr, "new_status", newStatus)
					} else {
						kycStatus = &newStatus
						// Update kycData with latest decision data
						kycData = decisionJSON
						if statusChanged {
							slog.Info("kyc status changed", "user_id", userID, "old_status", oldStatusStr, "new_status", newStatus, "didit_status", decision.Status)
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