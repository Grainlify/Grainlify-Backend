package soroban

import (
        "context"
        "encoding/json"
        "net/http"
        "net/http/httptest"
        "sync"
        "sync/atomic"
        "testing"
        "time"
)

func newTestClient(t *testing.T, rpcURL string) *Client {
        t.Helper()
        client, err := NewClient(Config{RPCURL: rpcURL})
        if err != nil {
                t.Fatalf("NewClient failed: %v", err)
        }
        return client
}

// TestPollTransactionStatus_ConcurrentCallsShareOneRPCCall verifies that
// concurrent pollers for the same tx hash result in only one underlying
// RPC call sequence, and that every caller gets the correct, identical
// final status.
func TestPollTransactionStatus_ConcurrentCallsShareOneRPCCall(t *testing.T) {
        var callCount int64

        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                atomic.AddInt64(&callCount, 1)
                resp := map[string]interface{}{
                        "jsonrpc": "2.0",
                        "id":      1,
                        "result": map[string]interface{}{
                                "status": "SUCCESS",
                                "hash":   "abc123",
                        },
                }
                body, _ := json.Marshal(resp)
                w.Header().Set("Content-Type", "application/json")
                w.Write(body)
        }))
        defer server.Close()

        client := newTestClient(t, server.URL)

        const numCallers = 20
        var wg sync.WaitGroup
        results := make([]map[string]interface{}, numCallers)
        errs := make([]error, numCallers)

        for i := 0; i < numCallers; i++ {
                wg.Add(1)
                go func(idx int) {
                        defer wg.Done()
                        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                        defer cancel()
                        status, err := client.PollTransactionStatus(ctx, "abc123", 5*time.Second)
                        results[idx] = status
                        errs[idx] = err
                }(i)
        }
        wg.Wait()

        for i, err := range errs {
                if err != nil {
                        t.Fatalf("caller %d got unexpected error: %v", i, err)
                }
                if results[i]["status"] != "SUCCESS" {
                        t.Fatalf("caller %d got unexpected status: %v", i, results[i])
                }
        }

        // The dedup layer should ensure a single shared poll loop for the
        // duration of these concurrent calls, so we expect a small, bounded
        // number of RPC calls (one per ticker tick of the shared poll), not
        // one per caller.
        got := atomic.LoadInt64(&callCount)
        if got >= numCallers {
                t.Fatalf("expected RPC calls to be deduplicated (want << %d), got %d", numCallers, got)
        }
        t.Logf("RPC calls made for %d concurrent callers: %d", numCallers, got)
}

// TestPollTransactionStatus_SequentialCallsAreIndependent ensures that once
// a poll for a hash resolves, a later call for the *same* hash starts a
// fresh poll rather than reusing a stale cached result forever.
func TestPollTransactionStatus_SequentialCallsAreIndependent(t *testing.T) {
        var callCount int64
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                atomic.AddInt64(&callCount, 1)
                resp := map[string]interface{}{
                        "jsonrpc": "2.0",
                        "id":      1,
                        "result":  map[string]interface{}{"status": "SUCCESS"},
                }
                body, _ := json.Marshal(resp)
                w.Write(body)
        }))
        defer server.Close()

        client := newTestClient(t, server.URL)
        ctx := context.Background()

        if _, err := client.PollTransactionStatus(ctx, "hash-1", 5*time.Second); err != nil {
                t.Fatalf("first call failed: %v", err)
        }
        firstCount := atomic.LoadInt64(&callCount)

        if _, err := client.PollTransactionStatus(ctx, "hash-1", 5*time.Second); err != nil {
                t.Fatalf("second call failed: %v", err)
        }
        secondCount := atomic.LoadInt64(&callCount)

        if secondCount <= firstCount {
                t.Fatalf("expected a fresh poll after the first resolved, call count did not increase (%d -> %d)", firstCount, secondCount)
        }

        if _, ok := client.inFlight["hash-1"]; ok {
                t.Fatalf("inFlight entry for hash-1 should have been cleaned up after resolution")
        }
}

// TestPollTransactionStatus_FollowerRespectsOwnDeadline ensures a follower
// with a shorter deadline than the leader's poll still returns promptly
// with its own context error, rather than being forced to wait for the
// (slower) leader.
func TestPollTransactionStatus_FollowerRespectsOwnDeadline(t *testing.T) {
        // Server never resolves to SUCCESS/FAILED, so the leader keeps polling
        // until its own timeout.
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                resp := map[string]interface{}{
                        "jsonrpc": "2.0",
                        "id":      1,
                        "result":  map[string]interface{}{"status": "PENDING"},
                }
                body, _ := json.Marshal(resp)
                w.Write(body)
        }))
        defer server.Close()

        client := newTestClient(t, server.URL)

        var wg sync.WaitGroup
        wg.Add(1)
        go func() {
                defer wg.Done()
                leaderCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
                defer cancel()
                _, _ = client.PollTransactionStatus(leaderCtx, "pending-hash", 10*time.Second)
        }()

        // Give the leader a moment to register itself as in-flight.
        time.Sleep(100 * time.Millisecond)

        followerCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
        defer cancel()

        start := time.Now()
        _, err := client.PollTransactionStatus(followerCtx, "pending-hash", 10*time.Second)
        elapsed := time.Since(start)

        if err == nil {
                t.Fatalf("expected follower to time out with its own shorter deadline, got no error")
        }
        if elapsed > 2*time.Second {
                t.Fatalf("follower took too long to respect its own deadline: %v", elapsed)
        }

        wg.Wait()
}
