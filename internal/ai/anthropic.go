// Package ai wraps the model calls GrainHack's assignment (AI-specs.md §4.3)
// and judging (§5) pipelines make. Kept deliberately thin: one Messages API
// call with a forced tool, because the spec requires structured output be
// enforced "via tool-use, not free-text parsing".
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// messagesURL is a var (not a const) so tests can point it at an
// httptest.Server, matching the internal/github package's convention.
var messagesURL = "https://api.anthropic.com/v1/messages"

const anthropicVersion = "2023-06-01"

// Client calls the Anthropic Messages API.
type Client struct {
	APIKey string
	HTTP   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{
		APIKey: apiKey,
		// Generous but bounded: a fit assessment is one small call, and a
		// hung request must not wedge a draw that's waiting on it.
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// Enabled reports whether the client can actually make a call. Callers use
// this to fall back to deterministic behaviour rather than erroring when no
// key is configured.
func (c *Client) Enabled() bool { return c != nil && c.APIKey != "" }

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type messagesRequest struct {
	Model      string           `json:"model"`
	MaxTokens  int              `json:"max_tokens"`
	System     string           `json:"system,omitempty"`
	Messages   []map[string]any `json:"messages"`
	Tools      []toolDef        `json:"tools,omitempty"`
	ToolChoice map[string]any   `json:"tool_choice,omitempty"`
}

type messagesResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// CallRecord is everything about one model call worth keeping. Returned
// even when the call fails, because a failed or malformed call is exactly
// what an appeal needs to see.
type CallRecord struct {
	Model       string
	RawRequest  []byte
	RawResponse []byte
	DurationMS  int
	// SchemaViolation is true when the model answered but not with a valid
	// tool call matching the schema. Distinct from a transport error: the
	// caller treats it as low confidence rather than as an outage.
	SchemaViolation bool
}

// StructuredCall runs one Messages request that is *forced* to answer by
// calling toolName with an input matching schema, and unmarshals that input
// into out.
//
// Forcing the tool is what makes the output a contract rather than a
// suggestion: there is no prose to parse, no markdown fences to strip, and a
// response that doesn't fit the schema fails loudly here instead of being
// silently half-parsed downstream.
func (c *Client) StructuredCall(
	ctx context.Context,
	model, system, userContent, toolName, toolDescription string,
	schema map[string]any,
	out any,
) error {
	_, err := c.StructuredCallRecorded(ctx, model, system, userContent, toolName, toolDescription, schema, out)
	return err
}

// StructuredCallRecorded is StructuredCall plus the raw request and response,
// for callers that must log every call permanently.
//
// The tool is *forced*, and a response that does not match the schema is
// reported as a SchemaViolation rather than repaired. There is deliberately
// no free-text fallback: a "repair" of a malformed judging response is a
// guess about what the model meant, made by code that cannot know, on a
// decision that moves money. Malformed means low confidence and a human.
func (c *Client) StructuredCallRecorded(
	ctx context.Context,
	model, system, userContent, toolName, toolDescription string,
	schema map[string]any,
	out any,
) (CallRecord, error) {
	rec := CallRecord{Model: model}
	if !c.Enabled() {
		return rec, fmt.Errorf("ai.StructuredCall: no API key configured")
	}
	body, err := json.Marshal(messagesRequest{
		Model:     model,
		MaxTokens: 2048,
		System:    system,
		Messages: []map[string]any{
			{"role": "user", "content": userContent},
		},
		Tools: []toolDef{{
			Name:        toolName,
			Description: toolDescription,
			InputSchema: schema,
		}},
		ToolChoice: map[string]any{"type": "tool", "name": toolName},
	})
	if err != nil {
		return rec, fmt.Errorf("ai.StructuredCall: marshal: %w", err)
	}
	rec.RawRequest = body

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, bytes.NewReader(body))
	if err != nil {
		return rec, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.HTTP.Do(req)
	rec.DurationMS = int(time.Since(started).Milliseconds())
	if err != nil {
		return rec, fmt.Errorf("ai.StructuredCall: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	rec.RawResponse = raw
	if readErr != nil {
		return rec, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return rec, fmt.Errorf("ai.StructuredCall: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		rec.SchemaViolation = true
		return rec, fmt.Errorf("ai.StructuredCall: decode: %w", err)
	}
	if mr.Error != nil {
		return rec, fmt.Errorf("ai.StructuredCall: %s: %s", mr.Error.Type, mr.Error.Message)
	}
	for _, block := range mr.Content {
		if block.Type == "tool_use" && block.Name == toolName {
			if err := json.Unmarshal(block.Input, out); err != nil {
				rec.SchemaViolation = true
				return rec, fmt.Errorf("ai.StructuredCall: tool input did not match the schema: %w", err)
			}
			return rec, nil
		}
	}
	rec.SchemaViolation = true
	return rec, fmt.Errorf("ai.StructuredCall: model returned no %s tool call (stop_reason=%s)", toolName, mr.StopReason)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
