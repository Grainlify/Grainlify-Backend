package soroban

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/xdr"
)

// newFakeProgramEscrowContract builds a ProgramEscrowContract with a real
// TransactionBuilder wired against srv, mirroring newFakeEscrowContract in
// escrow_test.go (same package) for tests that need BuildAndSubmit /
// WaitForConfirmation to actually run against a fake Horizon server.
func newFakeProgramEscrowContract(kp *keypair.Full, srv *httptest.Server) *ProgramEscrowContract {
	client := &Client{
		networkPassphrase: network.TestNetworkPassphrase,
		horizonClient: &horizonclient.Client{
			HorizonURL: srv.URL,
			HTTP:       srv.Client(),
		},
	}
	return &ProgramEscrowContract{
		client:          client,
		txBuilder:       &TransactionBuilder{client: client, sourceKP: kp},
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000002",
	}
}

// programScVal constructs a representative ScMap returned by get_program_info.
func programScVal(programID string, totalFunds, remainingBalance int64, authKey, tokenAddr string) xdr.ScVal {
	sym := func(s string) xdr.ScVal {
		sym := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	}
	i64 := func(v int64) xdr.ScVal {
		val := xdr.Int64(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}
	}
	str := func(s string) xdr.ScVal {
		v := xdr.ScString(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
	}
	addrVal := func(addr string) xdr.ScVal {
		v, err := EncodeScValAddress(addr)
		if err != nil {
			panic(err)
		}
		return v
	}

	entries := xdr.ScMap{
		{Key: sym("program_id"), Val: str(programID)},
		{Key: sym("total_funds"), Val: i64(totalFunds)},
		{Key: sym("remaining_balance"), Val: i64(remainingBalance)},
		{Key: sym("authorized_payout_key"), Val: addrVal(authKey)},
		{Key: sym("token_address"), Val: addrVal(tokenAddr)},
	}
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// buildSimulateResponseProgram reuses the test helpers from escrow_test.go
// (same package, no need to re-declare).

func newProgramTestServer(t *testing.T, scVal xdr.ScVal) *httptest.Server {
	t.Helper()
	b, err := scVal.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal ScVal: %v", err)
	}
	inner, _ := json.Marshal(simulateTransactionResponse{
		Results: []simulateResult{{XDR: base64.StdEncoding.EncodeToString(b)}},
	})
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  json.RawMessage(inner),
	}
	payload, _ := json.Marshal(body)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
}

func newProgramContract(t *testing.T, srv *httptest.Server) *ProgramEscrowContract {
	t.Helper()
	c, err := NewClient(Config{
		RPCURL:      srv.URL,
		Network:     NetworkTestnet,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &ProgramEscrowContract{
		client:          c,
		contractAddress: "0000000000000000000000000000000000000000000000000000000000000002",
	}
}

func TestGetProgramInfo_Success(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	authKey := kp.Address()

	const (
		programID        = "prog-123"
		totalFunds       = int64(10_000_000_000)
		remainingBalance = int64(7_500_000_000)
		// Use a contract hex address for tokenAddr to exercise contract branch.
		tokenAddr = "0000000000000000000000000000000000000000000000000000000000000099"
	)

	scVal := programScVal(programID, totalFunds, remainingBalance, authKey, tokenAddr)
	srv := newProgramTestServer(t, scVal)
	defer srv.Close()

	pec := newProgramContract(t, srv)
	data, err := pec.GetProgramInfo(context.Background())
	if err != nil {
		t.Fatalf("GetProgramInfo: %v", err)
	}

	if data.ProgramID != programID {
		t.Errorf("program_id: want %q, got %q", programID, data.ProgramID)
	}
	if data.TotalFunds != totalFunds {
		t.Errorf("total_funds: want %d, got %d", totalFunds, data.TotalFunds)
	}
	if data.RemainingBalance != remainingBalance {
		t.Errorf("remaining_balance: want %d, got %d", remainingBalance, data.RemainingBalance)
	}
}

func TestGetProgramInfo_RPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": -32600, "message": "internal error"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pec := newProgramContract(t, srv)
	_, err := pec.GetProgramInfo(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetProgramInfo_SimulationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(simulateTransactionResponse{Error: "out of gas"})
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  json.RawMessage(inner),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pec := newProgramContract(t, srv)
	_, err := pec.GetProgramInfo(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetProgramInfo_WrongScValType(t *testing.T) {
	val := xdr.Int64(42)
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}
	srv := newProgramTestServer(t, scVal)
	defer srv.Close()

	pec := newProgramContract(t, srv)
	_, err := pec.GetProgramInfo(context.Background())
	if err == nil {
		t.Fatal("expected error for wrong ScVal type, got nil")
	}
}

func TestGetRemainingBalance_Success(t *testing.T) {
	const balance = int64(7_500_000_000)
	val := xdr.Int64(balance)
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &val}
	srv := newProgramTestServer(t, scVal)
	defer srv.Close()

	pec := newProgramContract(t, srv)
	got, err := pec.GetRemainingBalance(context.Background())
	if err != nil {
		t.Fatalf("GetRemainingBalance: %v", err)
	}
	if got != balance {
		t.Errorf("balance: want %d, got %d", balance, got)
	}
}

func TestGetRemainingBalance_WrongType(t *testing.T) {
	str := xdr.ScString("unexpected")
	scVal := xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
	srv := newProgramTestServer(t, scVal)
	defer srv.Close()

	pec := newProgramContract(t, srv)
	_, err := pec.GetRemainingBalance(context.Background())
	if err == nil {
		t.Fatal("expected error for wrong ScVal type, got nil")
	}
}

func TestGetRemainingBalance_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	pec := newProgramContract(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pec.GetRemainingBalance(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestGetProgramInfo_InvalidContract(t *testing.T) {
	pec := &ProgramEscrowContract{
		contractAddress: "invalid_contract_addr",
	}
	_, err := pec.GetProgramInfo(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid contract, got nil")
	}
}

func TestGetRemainingBalance_InvalidContract(t *testing.T) {
	pec := &ProgramEscrowContract{
		contractAddress: "invalid_contract_addr",
	}
	_, err := pec.GetRemainingBalance(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid contract, got nil")
	}
}

// TestBatchPayout_ConfirmationFailureReturnsConfirmationUnknownError is the
// regression test for issue #297, covering BatchPayout as required by the
// issue's acceptance criteria: a nil error here would risk a caller
// recording a batch of payouts as delivered without on-chain confirmation.
// A WaitForConfirmation failure must return a non-nil error satisfying
// errors.Is(err, ErrConfirmationUnknown), while still returning the
// submitted TransactionResult (hash + "pending" status).
func TestBatchPayout_ConfirmationFailureReturnsConfirmationUnknownError(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const txHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	srv, _ := newFakeHorizonServer(t, kp.Address(), txHash, "")
	defer srv.Close()

	pec := newFakeProgramEscrowContract(kp, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := pec.BatchPayout(ctx, []PayoutItem{{Recipient: kp.Address(), Amount: 100}})
	if !errors.Is(err, ErrConfirmationUnknown) {
		t.Fatalf("BatchPayout error = %v, want errors.Is(err, ErrConfirmationUnknown)", err)
	}
	if result == nil || result.Hash != txHash {
		t.Fatalf("expected TransactionResult with hash %q, got %+v", txHash, result)
	}
	if result.Status != "pending" {
		t.Errorf("status: want %q, got %q", "pending", result.Status)
	}
}

// TestLockProgramFunds_ConfirmationFailureReturnsConfirmationUnknownError and
// TestSinglePayout_ConfirmationFailureReturnsConfirmationUnknownError round
// out coverage for the remaining two program-escrow fund-moving methods.
func TestLockProgramFunds_ConfirmationFailureReturnsConfirmationUnknownError(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const txHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	srv, _ := newFakeHorizonServer(t, kp.Address(), txHash, "")
	defer srv.Close()

	pec := newFakeProgramEscrowContract(kp, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := pec.LockProgramFunds(ctx, 100)
	if !errors.Is(err, ErrConfirmationUnknown) {
		t.Fatalf("LockProgramFunds error = %v, want errors.Is(err, ErrConfirmationUnknown)", err)
	}
	if result == nil || result.Hash != txHash {
		t.Fatalf("expected TransactionResult with hash %q, got %+v", txHash, result)
	}
}

func TestSinglePayout_ConfirmationFailureReturnsConfirmationUnknownError(t *testing.T) {
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const txHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	srv, _ := newFakeHorizonServer(t, kp.Address(), txHash, "")
	defer srv.Close()

	pec := newFakeProgramEscrowContract(kp, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := pec.SinglePayout(ctx, kp.Address(), 100)
	if !errors.Is(err, ErrConfirmationUnknown) {
		t.Fatalf("SinglePayout error = %v, want errors.Is(err, ErrConfirmationUnknown)", err)
	}
	if result == nil || result.Hash != txHash {
		t.Fatalf("expected TransactionResult with hash %q, got %+v", txHash, result)
	}
}
