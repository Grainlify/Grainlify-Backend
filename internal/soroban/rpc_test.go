package soroban

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go/xdr"
)

// ---------------------------------------------------------------------------
// Call
// ---------------------------------------------------------------------------

func TestCall_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Call(context.Background(), "getLatestLedger", nil)
	if err == nil {
		t.Fatal("Call() succeeded, want error for non-200 status")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("Call() error = %v, want it to mention the status code", err)
	}
}

func TestCall_RPCErrorPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32602, "message": "invalid params"},
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Call(context.Background(), "getLatestLedger", nil)
	if err == nil {
		t.Fatal("Call() succeeded, want error for a JSON-RPC error payload")
	}
	if !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("Call() error = %v, want it to mention the RPC error message", err)
	}
}

func TestCall_MalformedResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Call(context.Background(), "getLatestLedger", nil)
	if err == nil {
		t.Fatal("Call() succeeded, want a decode error for a malformed response body")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("Call() error = %v, want it to mention decoding failed", err)
	}
}

func TestCall_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server failed to decode request: %v", err)
		}
		if req.Method != "getLatestLedger" {
			t.Errorf("method = %q, want getLatestLedger", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"sequence": 12345},
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	resp, err := client.Call(context.Background(), "getLatestLedger", nil)
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["sequence"] != float64(12345) {
		t.Errorf("result[sequence] = %v, want 12345", result["sequence"])
	}
}

// ---------------------------------------------------------------------------
// SimulateAndDecode
// ---------------------------------------------------------------------------

// scValContractID is a valid contract address used for every SimulateAndDecode test.
const scValContractID = "0000000000000000000000000000000000000000000000000000000000000001"

func TestSimulateAndDecode_SimulationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		inner, _ := json.Marshal(simulateTransactionResponse{Error: "contract trapped"})
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  json.RawMessage(inner),
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	addr, err := EncodeContractAddress(scValContractID)
	if err != nil {
		t.Fatalf("EncodeContractAddress: %v", err)
	}

	_, err = client.SimulateAndDecode(context.Background(), addr, "get_balance", nil)
	if err == nil {
		t.Fatal("SimulateAndDecode() succeeded, want error for a simulation error response")
	}
	if !strings.Contains(err.Error(), "contract trapped") {
		t.Errorf("SimulateAndDecode() error = %v, want it to mention the simulation error", err)
	}
}

func TestSimulateAndDecode_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		inner, _ := json.Marshal(simulateTransactionResponse{Results: []simulateResult{}})
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  json.RawMessage(inner),
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	addr, err := EncodeContractAddress(scValContractID)
	if err != nil {
		t.Fatalf("EncodeContractAddress: %v", err)
	}

	_, err = client.SimulateAndDecode(context.Background(), addr, "get_balance", nil)
	if err == nil {
		t.Fatal("SimulateAndDecode() succeeded, want error for an empty results array")
	}
	if !strings.Contains(err.Error(), "no results") {
		t.Errorf("SimulateAndDecode() error = %v, want it to mention no results", err)
	}
}

func TestSimulateAndDecode_Success(t *testing.T) {
	want := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: func() *xdr.Int64 { v := xdr.Int64(42); return &v }()}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildSimulateResponse(t, want))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	addr, err := EncodeContractAddress(scValContractID)
	if err != nil {
		t.Fatalf("EncodeContractAddress: %v", err)
	}

	got, err := client.SimulateAndDecode(context.Background(), addr, "get_balance", nil)
	if err != nil {
		t.Fatalf("SimulateAndDecode() error = %v, want nil", err)
	}
	if got.Type != xdr.ScValTypeScvI64 || got.I64 == nil || *got.I64 != 42 {
		t.Errorf("SimulateAndDecode() = %+v, want i64(42)", got)
	}
}

// ---------------------------------------------------------------------------
// PollTransactionStatus in-flight dedup
// ---------------------------------------------------------------------------

// pendingThenSuccessServer serves getTransaction requests: the first call
// returns SUCCESS immediately. It counts every call so a test can verify
// exactly how many independent HTTP round-trips occurred.
func pendingThenSuccessServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"status": "SUCCESS"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestPollTransactionStatus_ConcurrentCallersShareOnePoll is the regression
// test for the dedup contract documented on Client.inFlight: two concurrent
// callers polling the same txHash must share a single underlying poll loop
// (one leader, one follower), not run two independent loops. The server
// always resolves on its very first getTransaction call, so if dedup works,
// exactly one HTTP call happens total regardless of how many concurrent
// callers are waiting; if dedup is broken, each of the two callers runs its
// own poll loop and each makes its own (successful) first call, for two
// total calls.
func TestPollTransactionStatus_ConcurrentCallersShareOnePoll(t *testing.T) {
	srv, calls := pendingThenSuccessServer(t)

	client, err := NewClient(Config{RPCURL: srv.URL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	const txHash = "concurrent-dedup-hash"
	var wg sync.WaitGroup
	results := make([]map[string]interface{}, 2)
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = client.PollTransactionStatus(context.Background(), txHash, 5*time.Second)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: PollTransactionStatus() error = %v, want nil", i, err)
		}
	}
	if results[0]["status"] != results[1]["status"] {
		t.Errorf("callers received different results: %v vs %v", results[0], results[1])
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("getTransaction call count = %d, want exactly 1 (concurrent callers should share one poll loop)", got)
	}
}

// pendingForeverServer serves getTransaction requests that never reach a
// terminal status, so a leader's poll never resolves on its own within the
// test's timeframe -- used to test that a follower with a shorter deadline
// than the leader still returns promptly via its own context.
func pendingForeverServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  map[string]interface{}{"status": "PENDING"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPollTransactionStatus_FollowerShortDeadlineReturnsPromptly is the
// regression test for the second half of the dedup contract documented on
// PollTransactionStatus: "Each follower still honors its own ctx
// cancellation/deadline independently of the leader." A follower whose own
// context has a much shorter deadline than the leader's overall timeout (and
// shorter than the 2s poll interval) must return ctx.Err() promptly, not
// block until the leader's poll resolves or times out.
func TestPollTransactionStatus_FollowerShortDeadlineReturnsPromptly(t *testing.T) {
	srv := pendingForeverServer(t)

	client, err := NewClient(Config{RPCURL: srv.URL, Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	const txHash = "short-deadline-follower-hash"

	// Leader: long enough to still be in flight when the follower checks in,
	// short enough not to leak past this test's own lifetime.
	go func() {
		_, _ = client.PollTransactionStatus(context.Background(), txHash, 3*time.Second)
	}()
	time.Sleep(20 * time.Millisecond) // let the leader register itself first

	followerCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.PollTransactionStatus(followerCtx, txHash, 3*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("follower error = %v, want context.DeadlineExceeded", err)
	}
	// Well under the leader's 2s poll interval and 3s overall timeout --
	// proves the follower returned via its own context, not the leader's.
	if elapsed > 1*time.Second {
		t.Fatalf("follower took %v to return, want well under 1s (its own deadline was 100ms)", elapsed)
	}
}
