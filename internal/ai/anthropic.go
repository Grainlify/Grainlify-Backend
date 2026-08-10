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
	if !c.Enabled() {
		return fmt.Errorf("ai.StructuredCall: no API key configured")
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
		return fmt.Errorf("ai.StructuredCall: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ai.StructuredCall: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ai.StructuredCall: http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return fmt.Errorf("ai.StructuredCall: decode: %w", err)
	}
	if mr.Error != nil {
		return fmt.Errorf("ai.StructuredCall: %s: %s", mr.Error.Type, mr.Error.Message)
	}
	for _, block := range mr.Content {
		if block.Type == "tool_use" && block.Name == toolName {
			if err := json.Unmarshal(block.Input, out); err != nil {
				return fmt.Errorf("ai.StructuredCall: decode tool input: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("ai.StructuredCall: model returned no %s tool call (stop_reason=%s)", toolName, mr.StopReason)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
