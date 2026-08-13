package calibration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// A minimal chat-completions call owned by the calibration harness.
//
// Not internal/ai's client, for one specific reason found by running it:
// gpt-5.6-luna refuses function tools unless reasoning_effort is explicitly
// "none" -
//
//	Function tools with reasoning_effort are not supported for gpt-5.6-luna
//	in /v1/chat/completions. To use function tools, use /v1/responses or set
//	reasoning_effort to 'none'.
//
// The shared client does not send the field at all, so the model applies its
// default and rejects the request. Teaching the shared client about it would
// mean changing the code path production judging runs on, tonight, to satisfy
// a local benchmark - so the harness carries its own call instead. The prompt
// and the output contract are still the production ones; only the transport is
// local.
type openAICall struct {
	APIKey string
	HTTP   *http.Client
}

type chatRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Tools    []map[string]any `json:"tools"`
	// ToolChoice forces the function call, so a verdict can never arrive as
	// prose that something downstream has to parse hopefully.
	ToolChoice map[string]any `json:"tool_choice"`
	// ReasoningEffort is sent explicitly rather than left to the model's
	// default. See the type comment.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *openAICall) structured(ctx context.Context, model, effort, system, user, toolName string, schema map[string]any, out any) (promptTok, completionTok int, err error) {
	reqBody, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":       toolName,
				"parameters": schema,
			},
		}},
		ToolChoice:      map[string]any{"type": "function", "function": map[string]any{"name": toolName}},
		ReasoningEffort: effort,
	})
	if err != nil {
		return 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, 0, fmt.Errorf("unparseable response (http %d): %s", resp.StatusCode, truncateForError(string(raw)))
	}
	if parsed.Error != nil {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, fmt.Errorf("openai: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, fmt.Errorf("openai http %d: %s", resp.StatusCode, truncateForError(string(raw)))
	}
	if len(parsed.Choices) == 0 || len(parsed.Choices[0].Message.ToolCalls) == 0 {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens,
			fmt.Errorf("model answered without calling %s; a verdict that is not a tool call is not a verdict", toolName)
	}
	args := parsed.Choices[0].Message.ToolCalls[0].Function.Arguments
	if err := json.Unmarshal([]byte(args), out); err != nil {
		return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, fmt.Errorf("tool arguments did not match the schema: %w", err)
	}
	return parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

func truncateForError(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func newOpenAICall(apiKey string) *openAICall {
	return &openAICall{APIKey: apiKey, HTTP: &http.Client{Timeout: 180 * time.Second}}
}
