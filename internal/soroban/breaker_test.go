package soroban

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	b := newCircuitBreaker(3, time.Minute)

	for i := 0; i < 3; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow() unexpectedly blocked before threshold reached: %v", err)
		}
		b.RecordFailure()
	}

	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen after %d consecutive failures", err, 3)
	}
}

func TestCircuitBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := newCircuitBreaker(3, time.Minute)

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() = %v, want nil: a success should reset the consecutive-failure count", err)
	}
}

func TestCircuitBreaker_HalfOpenProbeRecovers(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Millisecond)

	b.RecordFailure() // trips the breaker open
	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen while cooldown has not elapsed", err)
	}

	time.Sleep(15 * time.Millisecond)

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() = %v, want nil: cooldown elapsed, a half-open probe should be let through", err)
	}
	// A second concurrent caller must not get its own probe.
	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen: only one half-open probe may be in flight", err)
	}

	b.RecordSuccess()

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() = %v, want nil: a successful probe should close the breaker", err)
	}
}

func TestCircuitBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	b := newCircuitBreaker(1, 10*time.Millisecond)

	b.RecordFailure() // trips open
	time.Sleep(15 * time.Millisecond)

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() = %v, want nil for the half-open probe", err)
	}
	b.RecordFailure() // probe fails

	if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen: a failed probe should reopen the breaker", err)
	}
}

// TestClient_Call_CircuitBreakerFailsFast simulates consecutive RPC failures
// against a real Client.Call and asserts the breaker opens and fails fast
// without hitting the server, then recovers once the server starts
// succeeding again and the cooldown has elapsed.
func TestClient_Call_CircuitBreakerFailsFast(t *testing.T) {
	var (
		hits    int32
		failing atomic.Bool
	)
	failing.Store(true)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer server.Close()

	client, err := NewClient(Config{
		RPCURL:                  server.URL,
		Network:                 NetworkTestnet,
		CircuitBreakerThreshold: 2,
		CircuitBreakerCooldown:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := client.Call(ctx, "getLatestLedger", nil); err == nil {
			t.Fatalf("Call() succeeded, want failure from the stubbed 500 response")
		}
	}

	hitsBeforeOpen := atomic.LoadInt32(&hits)

	if _, err := client.Call(ctx, "getLatestLedger", nil); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Call() = %v, want ErrCircuitOpen once the breaker has tripped", err)
	}
	if got := atomic.LoadInt32(&hits); got != hitsBeforeOpen {
		t.Fatalf("server received a request while the breaker was open: hits went from %d to %d", hitsBeforeOpen, got)
	}

	time.Sleep(25 * time.Millisecond)
	failing.Store(false)

	if _, err := client.Call(ctx, "getLatestLedger", nil); err != nil {
		t.Fatalf("Call() = %v, want nil: the half-open probe should succeed once the server recovers", err)
	}
	if _, err := client.Call(ctx, "getLatestLedger", nil); err != nil {
		t.Fatalf("Call() = %v, want nil: the breaker should be fully closed again after a successful probe", err)
	}
}
