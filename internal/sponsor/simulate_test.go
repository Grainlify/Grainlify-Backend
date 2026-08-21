package sponsor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// FAIL OPEN. A simulation we cannot run is not evidence of a bad claim.
//
// If an unreachable node produced "this would fail", our own outage would refuse
// a contributor their money - and it would do it in the contract's voice. The
// caller must be able to tell "the chain says no" from "we could not ask", so
// they are different errors and the second never looks like the first.
func TestSimulateClaim_AnUnreachableNodeIsNotAnAbort(t *testing.T) {
	// A server that is closed before use: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	sim, err := NewSimulator(url).SimulateClaim(context.Background(), ClaimArgs{})
	if !errors.Is(err, ErrSimulationUnavailable) {
		t.Fatalf("want ErrSimulationUnavailable, got %v", err)
	}
	if errors.Is(err, ErrWouldAbort) {
		t.Error("our own outage was reported as the claim being bad")
	}
	if sim.Success {
		t.Error("an unreachable node produced a successful simulation")
	}
}

// A non-200 from the node is also us being unable to ask, not the contract
// speaking.
func TestSimulateClaim_ANodeErrorIsNotAnAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("upstream unavailable"))
	}))
	defer srv.Close()
	if _, err := NewSimulator(srv.URL).SimulateClaim(context.Background(), ClaimArgs{}); !errors.Is(err, ErrSimulationUnavailable) {
		t.Fatalf("want ErrSimulationUnavailable, got %v", err)
	}
}

// A simulated abort IS evidence, and must come back as a clean result rather
// than an error: the caller needs the vm_status to tell the person why.
func TestSimulateClaim_AnAbortIsAResultNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"success":false,"gas_used":"49","vm_status":"Move abort ... E_ALREADY_CLAIMED(0x8)"}]`))
	}))
	defer srv.Close()
	sim, err := NewSimulator(srv.URL).SimulateClaim(context.Background(), ClaimArgs{})
	if err != nil {
		t.Fatalf("an abort was returned as an error: %v", err)
	}
	if sim.Success {
		t.Fatal("an aborting simulation reported success")
	}
	if !contains(sim.VMStatus, "E_ALREADY_CLAIMED") {
		t.Errorf("the abort reason was lost: %q", sim.VMStatus)
	}
}

func TestSimulateClaim_ASuccessCarriesTheGas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"success":true,"gas_used":"4431","vm_status":"Executed successfully"}]`))
	}))
	defer srv.Close()
	sim, err := NewSimulator(srv.URL).SimulateClaim(context.Background(), ClaimArgs{})
	if err != nil || !sim.Success || sim.GasUsed != 4431 {
		t.Fatalf("sim=%+v err=%v", sim, err)
	}
}

// The request must carry a ZERO signature and the sender's PUBLIC KEY - that is
// what lets this run before the contributor signs.
func TestSimulateClaim_SendsAZeroSignatureAndTheRealPublicKey(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		body = string(b)
		w.Write([]byte(`[{"success":true,"gas_used":"1","vm_status":"ok"}]`))
	}))
	defer srv.Close()
	_, err := NewSimulator(srv.URL).SimulateClaim(context.Background(), ClaimArgs{SenderPubKey: "0xdeadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(body, "fee_payer_signature") {
		t.Error("not simulated as a fee-payer transaction")
	}
	if !contains(body, "0xdeadbeef") {
		t.Error("the sender's public key was not sent; the node needs it to match the auth key")
	}
	if !contains(body, zeroSig) {
		t.Error("a non-zero signature was sent; this must work before the contributor signs")
	}
}
