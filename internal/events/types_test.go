package events

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestEventTypesRoundTripJSON(t *testing.T) {
	tests := []struct {
		name string
		in   any
		out  any
	}{
		{
			name: "GitHubWebhookReceived",
			in: GitHubWebhookReceived{
				DeliveryID:   "delivery-123",
				Event:        "pull_request",
				Action:       "opened",
				RepoFullName: "Grainlify/Grainlify-Backend",
				Payload:      json.RawMessage(`{"action":"opened","repository":{"full_name":"Grainlify/Grainlify-Backend"}}`),
			},
			out: &GitHubWebhookReceived{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := json.Unmarshal(b, tt.out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			got := reflect.ValueOf(tt.out).Elem().Interface()
			if !reflect.DeepEqual(got, tt.in) {
				t.Fatalf("round trip mismatch\ngot:  %#v\nwant: %#v", got, tt.in)
			}
		})
	}
}

func TestGitHubWebhookReceivedUnmarshalIgnoresExtraFields(t *testing.T) {
	input := []byte(`{
		"delivery_id":"delivery-123",
		"event":"pull_request",
		"action":"opened",
		"repo_full_name":"Grainlify/Grainlify-Backend",
		"payload":{"action":"opened"},
		"future_field":"ignored"
	}`)

	var got GitHubWebhookReceived
	if err := json.Unmarshal(input, &got); err != nil {
		t.Fatalf("unmarshal with extra field: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate with extra field: %v", err)
	}

	want := GitHubWebhookReceived{
		DeliveryID:   "delivery-123",
		Event:        "pull_request",
		Action:       "opened",
		RepoFullName: "Grainlify/Grainlify-Backend",
		Payload:      json.RawMessage(`{"action":"opened"}`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extra field changed decoded event\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestGitHubWebhookReceivedValidateMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		json string
		want error
	}{
		{name: "delivery_id", json: `{"event":"pull_request","payload":{}}`, want: ErrMissingDeliveryID},
		{name: "event", json: `{"delivery_id":"delivery-123","payload":{}}`, want: ErrMissingEvent},
		{name: "payload", json: `{"delivery_id":"delivery-123","event":"pull_request"}`, want: ErrMissingPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got GitHubWebhookReceived
			if err := json.Unmarshal([]byte(tt.json), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := got.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.want)
			}
		})
	}
}
