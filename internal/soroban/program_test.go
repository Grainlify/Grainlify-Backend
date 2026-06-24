package soroban

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go/xdr"
)

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
	b, err := xdr.SafeMarshal(scVal)
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
	const (
		programID        = "prog-123"
		totalFunds       = int64(10_000_000_000)
		remainingBalance = int64(7_500_000_000)
		authKey          = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN"
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
