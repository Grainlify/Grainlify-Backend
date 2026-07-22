package soroban

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/xdr"
)

// buildSimulateResponse creates a JSON-RPC response body for simulateTransaction
// whose results[0].xdr contains the base64-encoded ScVal v.
func buildSimulateResponse(t *testing.T, v xdr.ScVal) []byte {
	t.Helper()
	b, err := v.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal ScVal: %v", err)
	}
	inner, _ := json.Marshal(simulateTransactionResponse{
		Results: []simulateResult{{XDR: base64.StdEncoding.EncodeToString(b)}},
	})
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  json.RawMessage(inner),
	}
	out, _ := json.Marshal(resp)
	return out
}

// newTestClient creates a Client pointing at srv with a 5 s timeout.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(Config{
		RPCURL:      srv.URL,
		Network:     NetworkTestnet,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// escrowScVal constructs a representative ScMap returned by get_escrow_info.
func escrowScVal(depositorAddress string, amount, deadline int64, status string) xdr.ScVal {
	sym := func(s string) xdr.ScVal {
		sym := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	}
	i64 := func(v int64) xdr.ScVal {
		val := xdr.Int64(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}
	}
	addrVal := func(addr string) xdr.ScVal {
		v, err := EncodeScValAddress(addr)
		if err != nil {
			panic(err)
		}
		return v
	}

	statusSym := xdr.ScSymbol(status)
	statusVal := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &statusSym}

	entries := xdr.ScMap{
		{Key: sym("depositor"), Val: addrVal(depositorAddress)},
		{Key: sym("amount"), Val: i64(amount)},
		{Key: sym("status"), Val: statusVal},
		{Key: sym("deadline"), Val: i64(deadline)},
	}
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func TestGetEscrowInfo_Success(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	depositor := kp.Address()
	const amount = int64(500_000_000)
	const deadline = int64(1_700_000_000)
	const status = "Locked"

	scVal := escrowScVal(depositor, amount, deadline, status)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSimulateResponse(t, scVal))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	data, err := ec.GetEscrowInfo(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetEscrowInfo: %v", err)
	}

	if data.Amount != amount {
		t.Errorf("amount: want %d, got %d", amount, data.Amount)
	}
	if data.Deadline != deadline {
		t.Errorf("deadline: want %d, got %d", deadline, data.Deadline)
	}
	if data.Status != EscrowStatus(status) {
		t.Errorf("status: want %q, got %q", status, data.Status)
	}
}

func TestGetEscrowInfo_RPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32600, "message": "contract not found"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(errResp)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := ec.GetEscrowInfo(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetEscrowInfo_SimulationError(t *testing.T) {
	// simulateTransaction succeeds at the HTTP level but returns an error field.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(simulateTransactionResponse{Error: "contract panicked"})
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  json.RawMessage(inner),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := ec.GetEscrowInfo(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetEscrowInfo_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(simulateTransactionResponse{Results: nil})
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  json.RawMessage(inner),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := ec.GetEscrowInfo(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for empty results, got nil")
	}
}

func TestGetEscrowInfo_WrongScValType(t *testing.T) {
	// Return an ScvI64 instead of ScvMap – should fail decoding.
	val := xdr.Int64(99)
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSimulateResponse(t, scVal))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := ec.GetEscrowInfo(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for wrong ScVal type, got nil")
	}
}

func TestGetBalance_Success(t *testing.T) {
	const balance = int64(1_000_000_000)
	val := xdr.Int64(balance)
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSimulateResponse(t, scVal))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	got, err := ec.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if got != balance {
		t.Errorf("balance: want %d, got %d", balance, got)
	}
}

func TestGetBalance_WrongType(t *testing.T) {
	str := xdr.ScString("not a number")
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildSimulateResponse(t, scVal))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	_, err := ec.GetBalance(context.Background())
	if err == nil {
		t.Fatal("expected error for wrong ScVal type, got nil")
	}
}

func TestGetBalance_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow response.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	ec := &EscrowContract{
		client:          client,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := ec.GetBalance(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestGetEscrowInfo_InvalidContract(t *testing.T) {
	ec := &EscrowContract{
		contractAddress: "invalid_contract_addr",
	}
	_, err := ec.GetEscrowInfo(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for invalid contract, got nil")
	}
}

func TestGetBalance_InvalidContract(t *testing.T) {
	ec := &EscrowContract{
		contractAddress: "invalid_contract_addr",
	}
	_, err := ec.GetBalance(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid contract, got nil")
	}
}

// newFakeHorizonServer serves the Horizon REST endpoints TransactionBuilder
// depends on: account lookup (for sequence numbers), transaction submission,
// and transaction lookup. If confirmHash is non-empty, GET /transactions/{confirmHash}
// succeeds and the returned counter tracks how many times that happens;
// otherwise it 404s, as Horizon does for a not-yet-confirmed transaction.
func newFakeHorizonServer(t *testing.T, sourceAddress, submitHash, confirmHash string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var txDetailCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/accounts/"):
			json.NewEncoder(w).Encode(map[string]any{
				"account_id": sourceAddress,
				"sequence":   "100",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/transactions":
			json.NewEncoder(w).Encode(map[string]any{
				"hash":   submitHash,
				"ledger": 500,
			})
		case confirmHash != "" && r.Method == http.MethodGet && r.URL.Path == "/transactions/"+confirmHash:
			txDetailCalls.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"hash":   confirmHash,
				"ledger": 501,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"status": http.StatusNotFound, "title": "Resource Missing"})
		}
	}))
	return srv, &txDetailCalls
}

func newFakeEscrowContract(kp *keypair.Full, srv *httptest.Server) *EscrowContract {
	client := &Client{
		networkPassphrase: network.TestNetworkPassphrase,
		horizonClient: &horizonclient.Client{
			HorizonURL: srv.URL,
			HTTP:       srv.Client(),
		},
	}
	return &EscrowContract{
		client:          client,
		txBuilder:       &TransactionBuilder{client: client, sourceKP: kp},
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000001",
	}
}

func TestInit_WaitsForConfirmation(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const txHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	srv, txDetailCalls := newFakeHorizonServer(t, kp.Address(), txHash, txHash)
	defer srv.Close()

	ec := newFakeEscrowContract(kp, srv)

	result, err := ec.Init(context.Background(), kp.Address(), kp.Address())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if txDetailCalls.Load() == 0 {
		t.Fatal("Init did not call WaitForConfirmation: no GET /transactions/{hash} request observed")
	}
	if result.Status != "success" {
		t.Errorf("status: want %q (confirmed), got %q", "success", result.Status)
	}
	if result.Ledger != 501 {
		t.Errorf("ledger: want 501 (from confirmed lookup), got %d", result.Ledger)
	}
}

func TestInit_ConfirmationFailureReturnsSubmissionResult(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const txHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	srv, _ := newFakeHorizonServer(t, kp.Address(), txHash, "")
	defer srv.Close()

	ec := newFakeEscrowContract(kp, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := ec.Init(ctx, kp.Address(), kp.Address())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.Hash != txHash {
		t.Errorf("hash: want %q, got %q", txHash, result.Hash)
	}
	if result.Status != "pending" {
		t.Errorf("status: want %q (unconfirmed submission result), got %q", "pending", result.Status)
	}
}
