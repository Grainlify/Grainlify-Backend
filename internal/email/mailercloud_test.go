package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withMailerCloudTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	original := mailerCloudAPIURL
	mailerCloudAPIURL = server.URL
	t.Cleanup(func() { mailerCloudAPIURL = original })
}

func TestNewMailerCloudMailer_NilWhenUnconfigured(t *testing.T) {
	if m := NewMailerCloudMailer("", "from@example.com", "Grainlify"); m != nil {
		t.Error("expected nil mailer when apiKey is empty")
	}
	if m := NewMailerCloudMailer("key", "", "Grainlify"); m != nil {
		t.Error("expected nil mailer when from is empty")
	}
}

func TestMailerCloudMailer_Send(t *testing.T) {
	var gotAuth string
	var gotBody mailerCloudSendRequest

	withMailerCloudTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(mailerCloudResponse{Status: "SUCCESS", StatusCode: 1000, Message: "NA"})
	})

	m := NewMailerCloudMailer("test-api-key", "notifications@grainlify.dev", "Grainlify")
	err := m.Send(context.Background(), "user@example.com", "You've been assigned", "<p>Hello</p>")
	if err != nil {
		t.Fatalf("Send: unexpected error: %v", err)
	}

	if gotAuth != "test-api-key" {
		t.Errorf("Authorization header = %q, want plain API key with no Bearer prefix", gotAuth)
	}
	if gotBody.Version != "1.0" {
		t.Errorf("version = %q, want 1.0", gotBody.Version)
	}
	if gotBody.Email.From != "notifications@grainlify.dev" {
		t.Errorf("from = %q, want notifications@grainlify.dev", gotBody.Email.From)
	}
	if gotBody.Email.FromName != "Grainlify" {
		t.Errorf("fromName = %q, want Grainlify", gotBody.Email.FromName)
	}
	if gotBody.Email.Subject != "You've been assigned" {
		t.Errorf("subject = %q", gotBody.Email.Subject)
	}
	if gotBody.Email.HTML != "<p>Hello</p>" {
		t.Errorf("html = %q", gotBody.Email.HTML)
	}
	if len(gotBody.Email.Recipients.To) != 1 || gotBody.Email.Recipients.To[0].Email != "user@example.com" {
		t.Errorf("recipients.to = %+v, want one entry for user@example.com", gotBody.Email.Recipients.To)
	}
}

func TestMailerCloudMailer_Send_APIErrorStatus(t *testing.T) {
	withMailerCloudTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Mailercloud can return HTTP 200 with a body-level ERROR status,
		// per their documented error response shape - not just non-2xx.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(mailerCloudResponse{Status: "ERROR", StatusCode: 9022, Message: "Unsupported version"})
	})

	m := NewMailerCloudMailer("test-api-key", "notifications@grainlify.dev", "Grainlify")
	if err := m.Send(context.Background(), "user@example.com", "Subject", "<p>Body</p>"); err == nil {
		t.Fatal("expected an error when the response body reports status ERROR, got nil")
	}
}

func TestMailerCloudMailer_Send_NonOKStatusCode(t *testing.T) {
	withMailerCloudTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(mailerCloudResponse{Status: "ERROR", StatusCode: 401, Message: "invalid api key"})
	})

	m := NewMailerCloudMailer("wrong-key", "notifications@grainlify.dev", "Grainlify")
	if err := m.Send(context.Background(), "user@example.com", "Subject", "<p>Body</p>"); err == nil {
		t.Fatal("expected an error for a 401 response, got nil")
	}
}
