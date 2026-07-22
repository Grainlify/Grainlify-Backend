package didit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const BaseURL = "https://verification.didit.me/v2"

var ErrMalformedResponse = errors.New("malformed didit response")

// APIError represents a non-2xx response from the Didit API.
// The raw response body is available via Body but is intentionally
// excluded from Error() to prevent PII leaking into logs.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("didit API error: status %d, error: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("didit API error: status %d", e.StatusCode)
}

type Client struct {
	HTTP         *http.Client
	APIKey       string
	UserAgent    string
	BaseURL      string
	PollInterval time.Duration
	MaxPolls     int
}

func NewClient(apiKey string) *Client {
	return &Client{
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		APIKey:       apiKey,
		UserAgent:    "grainlify-backend",
		BaseURL:      BaseURL,
		PollInterval: 2 * time.Second,
		MaxPolls:     15,
	}
}

// CreateSessionRequest is the request body for creating a verification session
type CreateSessionRequest struct {
	WorkflowID string `json:"workflow_id"`
	VendorData string `json:"vendor_data,omitempty"` // User ID or other identifier
	Callback   string `json:"callback,omitempty"`    // Webhook callback URL
}

// CreateSessionResponse is the response from creating a session
type CreateSessionResponse struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"` // Verification link for user
}

// CreateSession creates a new KYC verification session
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (CreateSessionResponse, error) {
	url := strings.TrimRight(c.baseURL(), "/") + "/session/"

	body, err := json.Marshal(req)
	if err != nil {
		return CreateSessionResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return CreateSessionResponse{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return CreateSessionResponse{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Read the full response body for error details
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return CreateSessionResponse{}, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse error response
		var errBody struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		_ = json.Unmarshal(bodyBytes, &errBody)

		// Build error message with all available information
		errMsg := errBody.Error
		if errMsg == "" {
			errMsg = errBody.Message
		}
		if errMsg == "" {
			errMsg = errBody.Detail
		}
		if errMsg == "" {
			errMsg = "unexpected error"
		}

		return CreateSessionResponse{}, &APIError{StatusCode: resp.StatusCode, Message: errMsg, Body: string(bodyBytes)}
	}

	var result CreateSessionResponse
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return CreateSessionResponse{}, &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("decode response: %s", err), Body: string(bodyBytes)}
	}

	return result, nil
}

// SessionDecisionResponse contains the verification decision/result
type SessionDecisionResponse struct {
	Status      string                 `json:"status"` // approved, rejected, pending, etc.
	Decision    map[string]interface{} `json:"decision,omitempty"`
	Data        map[string]interface{} `json:"data,omitempty"`
	RawResponse string                 `json:"-"` // Raw JSON response for debugging
	// Capture any additional fields that might be in the response
	ExtraFields map[string]interface{} `json:"-"`
}

// GetSessionDecision retrieves the verification decision for a session and polls until Didit returns a terminal status.
func (c *Client) GetSessionDecision(ctx context.Context, sessionID string) (SessionDecisionResponse, error) {
	maxPolls := c.MaxPolls
	if maxPolls <= 0 {
		maxPolls = 1
	}

	var last SessionDecisionResponse
	for attempt := 0; attempt < maxPolls; attempt++ {
		decision, err := c.getSessionDecisionOnce(ctx, sessionID)
		if err != nil {
			return SessionDecisionResponse{}, err
		}
		if isTerminalStatus(decision.Status) {
			return decision, nil
		}
		last = decision

		if attempt == maxPolls-1 {
			break
		}
		if err := sleepContext(ctx, c.pollInterval()); err != nil {
			return SessionDecisionResponse{}, err
		}
	}

	return last, nil
}

func (c *Client) getSessionDecisionOnce(ctx context.Context, sessionID string) (SessionDecisionResponse, error) {
	url := fmt.Sprintf("%s/session/%s/decision/", strings.TrimRight(c.baseURL(), "/"), sessionID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SessionDecisionResponse{}, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return SessionDecisionResponse{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	// Read the raw response body for debugging
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return SessionDecisionResponse{}, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(bodyBytes, &errBody)
		return SessionDecisionResponse{}, &APIError{StatusCode: resp.StatusCode, Message: errBody.Error, Body: string(bodyBytes)}
	}

	// First, unmarshal into a generic map to capture all fields
	var rawMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		return SessionDecisionResponse{}, &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("decode response: %s", err), Body: string(bodyBytes)}
	}

	// Extract known fields
	result := SessionDecisionResponse{
		RawResponse: string(bodyBytes),
		ExtraFields: make(map[string]interface{}),
	}

	if status, ok := rawMap["status"].(string); ok && strings.TrimSpace(status) != "" {
		result.Status = status
	} else {
		return SessionDecisionResponse{}, fmt.Errorf("%w: missing status", ErrMalformedResponse)
	}
	if decision, ok := rawMap["decision"].(map[string]interface{}); ok {
		result.Decision = decision
	}
	if data, ok := rawMap["data"].(map[string]interface{}); ok {
		result.Data = data
	}

	// Store any other fields that might contain rejection reasons
	for k, v := range rawMap {
		if k != "status" && k != "decision" && k != "data" {
			result.ExtraFields[k] = v
		}
	}

	return result, nil
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return BaseURL
	}
	return c.BaseURL
}

func (c *Client) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return 2 * time.Second
	}
	return c.PollInterval
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "approved", "verified", "rejected", "declined", "expired", "error", "failed":
		return true
	default:
		return false
	}
}
