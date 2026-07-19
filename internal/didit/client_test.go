package didit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(server *httptest.Server) *Client {
	return &Client{
		HTTP:         server.Client(),
		APIKey:       "test-key",
		UserAgent:    "test-agent",
		BaseURL:      server.URL,
		PollInterval: time.Millisecond,
		MaxPolls:     5,
	}
}

func TestGetSessionDecisionPollsPendingUntilApproved(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/session-123/decision/" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("unexpected api key %q", got)
		}
		call := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			fmt.Fprint(w, `{"status":"pending"}`)
			return
		}
		fmt.Fprint(w, `{"status":"approved","decision":{"reason":"ok"},"provider_id":"abc"}`)
	}))
	defer server.Close()

	decision, err := newTestClient(server).GetSessionDecision(context.Background(), "session-123")
	if err != nil {
		t.Fatalf("GetSessionDecision returned error: %v", err)
	}
	if decision.Status != "approved" {
		t.Fatalf("expected approved, got %q", decision.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected polling to stop after terminal status in 2 calls, got %d", got)
	}
	if decision.ExtraFields["provider_id"] != "abc" {
		t.Fatalf("expected extra provider_id to be captured, got %#v", decision.ExtraFields)
	}
}

func TestGetSessionDecisionPollsPendingUntilRejected(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if call < 3 {
			fmt.Fprint(w, `{"status":"pending"}`)
			return
		}
		fmt.Fprint(w, `{"status":"rejected","data":{"id_verification":{"status":"failed"}}}`)
	}))
	defer server.Close()

	decision, err := newTestClient(server).GetSessionDecision(context.Background(), "session-456")
	if err != nil {
		t.Fatalf("GetSessionDecision returned error: %v", err)
	}
	if decision.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", decision.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected polling to stop after rejected terminal status in 3 calls, got %d", got)
	}
}

func TestGetSessionDecisionMalformedResponseReturnsTypedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"decision":{"result":"approved"}}`)
	}))
	defer server.Close()

	_, err := newTestClient(server).GetSessionDecision(context.Background(), "session-malformed")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse, got %v", err)
	}
}

func TestGetSessionDecisionHTTPErrorReturnsErrorAndStopsPolling(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":"provider unavailable"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newTestClient(server).GetSessionDecision(context.Background(), "session-error")
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("expected status 502 error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected HTTP error to stop polling after one call, got %d", got)
	}
}

func TestGetSessionDecisionTimeoutReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, `{"status":"approved"}`)
	}))
	defer server.Close()

	client := newTestClient(server)
	client.HTTP = &http.Client{Timeout: 5 * time.Millisecond}

	_, err := client.GetSessionDecision(context.Background(), "session-timeout")
	if err == nil || !strings.Contains(err.Error(), "http request") {
		t.Fatalf("expected timeout http request error, got %v", err)
	}
}

func TestGetSessionDecisionStopsAtMaxPollsWhenStillPending(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{"status":"pending"}`)
	}))
	defer server.Close()

	client := newTestClient(server)
	client.MaxPolls = 3
	decision, err := client.GetSessionDecision(context.Background(), "session-pending")
	if err != nil {
		t.Fatalf("GetSessionDecision returned error: %v", err)
	}
	if decision.Status != "pending" {
		t.Fatalf("expected last pending decision, got %q", decision.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected exactly max polls, got %d", got)
	}
}
