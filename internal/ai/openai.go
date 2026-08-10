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

// chatCompletionsURL is a var so tests can point it at an httptest.Server.
var chatCompletionsURL = "https://api.openai.com/v1/chat/completions"

// OpenAIClient calls OpenAI's Chat Completions API with a forced function
// call.
//
// **Deliberately a separate type with its own request building, transport
// handling and response parsing - it shares no code path with Client
// (Anthropic) beyond the inert CallRecord struct.**
//
// AI-specs.md §5.6 uses a second provider because "a model's second run
// repeats its own blind spots, so self-agreement is weak evidence.
// Cross-provider agreement is strong." That argument only holds if the
// failure modes are actually independent. A shared response parser or a
// shared retry wrapper would reintroduce a common cause: one bug there
// makes both providers wrong in the same way at the same time, and the
// cross-check goes back to manufacturing agreement rather than testing it.
//
// So: no shared parsing helper, no shared retry, no shared error mapping.
// The two are similar-looking on purpose and separate on purpose. If you
// find yourself factoring out the duplication between this file and
// anthropic.go, that is the thing this comment is asking you not to do.
type OpenAIClient struct {
	APIKey string
	HTTP   *http.Client
}

func NewOpenAIClient(apiKey string) *OpenAIClient {
	return &OpenAIClient{
		APIKey: apiKey,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *OpenAIClient) Enabled() bool { return c != nil && c.APIKey != "" }

type openAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIRequest struct {
	Model      string           `json:"model"`
	Messages   []map[string]any `json:"messages"`
	Tools      []openAITool     `json:"tools"`
	ToolChoice map[string]any   `json:"tool_choice"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
					// OpenAI returns arguments as a JSON *string*, unlike
					// Anthropic's structured object. Parsed here and nowhere
					// else.
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// StructuredCallRecorded runs one forced-function call and unmarshals the
// arguments into out, returning the raw exchange for the permanent log.
//
// Same contract as the Anthropic path - forced tool, no free-text fallback,
// a schema mismatch reported rather than repaired - reached by entirely
// separate code.
func (c *OpenAIClient) StructuredCallRecorded(
	ctx context.Context,
	model, system, userContent, toolName, toolDescription string,
	schema map[string]any,
	out any,
) (CallRecord, error) {
	rec := CallRecord{Model: model}
	if !c.Enabled() {
		return rec, fmt.Errorf("ai.OpenAI: no API key configured")
	}

	body, err := json.Marshal(openAIRequest{
		Model: model,
		Messages: []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": userContent},
		},
		Tools: []openAITool{{
			Type: "function",
			Function: openAIFunction{
				Name:        toolName,
				Description: toolDescription,
				Parameters:  schema,
			},
		}},
		ToolChoice: map[string]any{
			"type":     "function",
			"function": map[string]any{"name": toolName},
		},
	})
	if err != nil {
		return rec, fmt.Errorf("ai.OpenAI: marshal: %w", err)
	}
	rec.RawRequest = body

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL, bytes.NewReader(body))
	if err != nil {
		return rec, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	rec.DurationMS = int(time.Since(started).Milliseconds())
	if err != nil {
		return rec, fmt.Errorf("ai.OpenAI: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	rec.RawResponse = raw
	if readErr != nil {
		return rec, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return rec, fmt.Errorf("ai.OpenAI: http %d: %s", resp.StatusCode, openAITruncate(string(raw), 400))
	}

	var parsed openAIResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		rec.SchemaViolation = true
		return rec, fmt.Errorf("ai.OpenAI: decode: %w", err)
	}
	if parsed.Error != nil {
		return rec, fmt.Errorf("ai.OpenAI: %s: %s", parsed.Error.Type, parsed.Error.Message)
	}
	for _, choice := range parsed.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.Function.Name != toolName {
				continue
			}
			if err := json.Unmarshal([]byte(call.Function.Arguments), out); err != nil {
				rec.SchemaViolation = true
				return rec, fmt.Errorf("ai.OpenAI: function arguments did not match the schema: %w", err)
			}
			return rec, nil
		}
	}
	rec.SchemaViolation = true
	return rec, fmt.Errorf("ai.OpenAI: model returned no %s function call", toolName)
}

// openAITruncate is this file's own, rather than anthropic.go's truncate,
// for the same isolation reason as everything else here.
func openAITruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
