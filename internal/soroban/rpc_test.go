package soroban

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pendingRPCServer stubs a Soroban RPC endpoint that always reports a
// transaction as still pending (never SUCCESS/FAILED), so a polling loop
// hitting it would otherwise keep polling until its timeout fires.
func pendingRPCServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"status": "PENDING"},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestPollTransactionStatus_ShortTimeoutFiresPromptly guards against the bug
// this issue fixes: pollTransactionStatusOnce used to only check its
// deadline inside the `<-ticker.C` branch of a 2-second ticker, so a timeout
// shorter than 2s wasn't honored until the first tick — delaying the
// returned error by up to ~2s beyond what the caller requested.
func TestPollTransactionStatus_ShortTimeoutFiresPromptly(t *testing.T) {
	server := pendingRPCServer(t)

	client, err := NewClient(Config{RPCURL: server.URL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	const timeout = 300 * time.Millisecond
	start := time.Now()
	_, err = client.PollTransactionStatus(context.Background(), "deadbeef", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("PollTransactionStatus() succeeded, want a timeout error")
	}
	// Generous margin above `timeout` for scheduling jitter, while staying
	// well under the ~2s floor the pre-fix code would have imposed.
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("PollTransactionStatus() took %v to return, want well under 2s for a %v timeout", elapsed, timeout)
	}
}

// TestPollTransactionStatus_CallerContextCancelledReturnsContextError asserts
// that cancelling the caller's own context (independent of the timeout
// budget) is still reported via ctx.Err(), not the generic timeout error.
func TestPollTransactionStatus_CallerContextCancelledReturnsContextError(t *testing.T) {
	server := pendingRPCServer(t)

	client, err := NewClient(Config{RPCURL: server.URL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = client.PollTransactionStatus(ctx, "deadbeef", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PollTransactionStatus() error = %v, want context.Canceled", err)
	}
}

// TestPollTransactionStatus_ResolvesOnFinalStatus confirms the loop still
// returns successfully once a poll observes a terminal status, unchanged
// from before this fix.
func TestPollTransactionStatus_ResolvesOnFinalStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"status": "SUCCESS"},
		})
	}))
	defer server.Close()

	client, err := NewClient(Config{RPCURL: server.URL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Timeout comfortably spans the first 2s ticker interval so the success
	// response has a chance to be observed.
	status, err := client.PollTransactionStatus(context.Background(), "deadbeef", 5*time.Second)
	if err != nil {
		t.Fatalf("PollTransactionStatus() error = %v, want nil", err)
	}
	if status["status"] != "SUCCESS" {
		t.Fatalf("PollTransactionStatus() status = %v, want SUCCESS", status["status"])
	}
}
