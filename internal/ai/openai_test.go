package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type verdictOut struct {
	Bucket     string `json:"bucket"`
	Confidence string `json:"confidence"`
}

func stubOpenAI(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	original := chatCompletionsURL
	chatCompletionsURL = srv.URL
	t.Cleanup(func() {
		chatCompletionsURL = original
		srv.Close()
	})
}

func TestOpenAI_ForcesTheFunctionAndParsesArguments(t *testing.T) {
	stubOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		// The tool must be forced, not merely offered - otherwise the model
		// can answer in prose and the schema stops being a contract.
		choice, _ := req["tool_choice"].(map[string]any)
		if choice == nil || choice["type"] != "function" {
			t.Errorf("tool_choice = %v, want a forced function", req["tool_choice"])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"record_judgement","arguments":"{\"bucket\":\"accepted\",\"confidence\":\"high\"}"}}]},"finish_reason":"tool_calls"}]}`))
	})

	var out verdictOut
	rec, err := NewOpenAIClient("k").StructuredCallRecorded(context.Background(),
		"gpt-4o", "sys", "user", "record_judgement", "desc", map[string]any{"type": "object"}, &out)
	if err != nil {
		t.Fatalf("StructuredCallRecorded: %v", err)
	}
	if out.Bucket != "accepted" || out.Confidence != "high" {
		t.Errorf("out = %+v, want accepted/high", out)
	}
	// The raw exchange has to be captured for the permanent log.
	if len(rec.RawRequest) == 0 || len(rec.RawResponse) == 0 {
		t.Error("raw request/response not recorded")
	}
	if rec.SchemaViolation {
		t.Error("SchemaViolation set on a valid response")
	}
}

// Malformed arguments must be reported, never repaired - the whole point of
// forcing the schema is that a mismatch is a human-review signal.
func TestOpenAI_MalformedArgumentsAreASchemaViolation(t *testing.T) {
	stubOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"record_judgement","arguments":"{not json"}}]}}]}`))
	})

	var out verdictOut
	rec, err := NewOpenAIClient("k").StructuredCallRecorded(context.Background(),
		"gpt-4o", "sys", "user", "record_judgement", "desc", map[string]any{"type": "object"}, &out)
	if err == nil {
		t.Fatal("no error for malformed function arguments")
	}
	if !rec.SchemaViolation {
		t.Error("SchemaViolation not set - the caller can't tell this from an outage")
	}
	if len(rec.RawResponse) == 0 {
		t.Error("malformed response not captured - it is the evidence for the escalation")
	}
}

func TestOpenAI_NoFunctionCallIsASchemaViolation(t *testing.T) {
	stubOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[]},"finish_reason":"stop"}]}`))
	})
	var out verdictOut
	rec, err := NewOpenAIClient("k").StructuredCallRecorded(context.Background(),
		"gpt-4o", "sys", "user", "record_judgement", "desc", map[string]any{"type": "object"}, &out)
	if err == nil || !rec.SchemaViolation {
		t.Errorf("err = %v, SchemaViolation = %v; want an error and a violation", err, rec.SchemaViolation)
	}
}

func TestOpenAI_DisabledWithoutAKey(t *testing.T) {
	c := NewOpenAIClient("")
	if c.Enabled() {
		t.Error("Enabled() true with no key")
	}
	var out verdictOut
	if _, err := c.StructuredCallRecorded(context.Background(),
		"gpt-4o", "s", "u", "t", "d", nil, &out); err == nil {
		t.Error("no error calling without a key")
	}
}

func TestOpenAI_HTTPErrorIsNotASchemaViolation(t *testing.T) {
	stubOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	})
	var out verdictOut
	rec, err := NewOpenAIClient("k").StructuredCallRecorded(context.Background(),
		"gpt-4o", "s", "u", "record_judgement", "d", map[string]any{"type": "object"}, &out)
	if err == nil {
		t.Fatal("no error on a 429")
	}
	// An outage is not the model misbehaving, and conflating them would
	// send a transient failure to the calibration set as a bad verdict.
	if rec.SchemaViolation {
		t.Error("a transport failure was reported as a schema violation")
	}
}
