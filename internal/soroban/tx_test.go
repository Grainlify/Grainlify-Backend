package soroban

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go/keypair"
	"github.com/stellar/go/xdr"
)

// newTestTransactionBuilder returns a TransactionBuilder whose Horizon
// client points at horizonURL, using a fresh random keypair as the source
// account (never actually used to sign or submit anything in these tests).
func newTestTransactionBuilder(t *testing.T, horizonURL string) *TransactionBuilder {
	t.Helper()

	client, err := NewClient(Config{RPCURL: "http://localhost:0", Network: NetworkTestnet})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	client.GetHorizonClient().HorizonURL = horizonURL

	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random failed: %v", err)
	}

	tb, err := NewTransactionBuilder(client, kp.Seed(), RetryConfig{})
	if err != nil {
		t.Fatalf("NewTransactionBuilder failed: %v", err)
	}
	return tb
}

// TestWaitForConfirmation_ShortTimeoutFiresPromptly guards against the same
// deadline-checking bug fixed for pollTransactionStatusOnce (rpc_test.go):
// WaitForConfirmation used to only check its deadline inside the
// `<-ticker.C` branch of a 2-second ticker, so a timeout shorter than 2s
// wasn't honored until the first tick.
func TestWaitForConfirmation_ShortTimeoutFiresPromptly(t *testing.T) {
	// The transaction is never found, so TransactionDetail keeps erroring
	// and the loop would otherwise keep polling until timeout.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"not_found"}`))
	}))
	defer server.Close()

	tb := newTestTransactionBuilder(t, server.URL)

	const timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := tb.WaitForConfirmation(context.Background(), "deadbeef", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("WaitForConfirmation() succeeded, want a timeout error")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("WaitForConfirmation() took %v to return, want well under 2s for a %v timeout", elapsed, timeout)
	}
}

// TestWaitForConfirmation_CallerContextCancelledReturnsContextError asserts
// that cancelling the caller's own context (independent of the timeout
// budget) is still reported via ctx.Err().
func TestWaitForConfirmation_CallerContextCancelledReturnsContextError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"not_found"}`))
	}))
	defer server.Close()

	tb := newTestTransactionBuilder(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := tb.WaitForConfirmation(ctx, "deadbeef", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForConfirmation() error = %v, want context.Canceled", err)
	}
}

func TestEncodeContractAddress(t *testing.T) {
	// Build a known 32-byte hash used for success-case assertions.
	var knownBytes [32]byte
	for i := range knownBytes {
		knownBytes[i] = byte(i + 1)
	}

	var knownHash xdr.Hash
	copy(knownHash[:], knownBytes[:])

	knownContractID := xdr.ContractId(knownHash)
	knownAddress := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &knownContractID,
	}

	hexStr := hex.EncodeToString(knownBytes[:])
	b64Str := base64.StdEncoding.EncodeToString(knownBytes[:])

	// 16 zero bytes in base64 – decodes successfully but is only 16 bytes.
	wrongLenBytes := make([]byte, 16)
	wrongLenB64 := base64.StdEncoding.EncodeToString(wrongLenBytes)

	tests := []struct {
		name     string
		input    string
		wantAddr xdr.ScAddress
		wantErr  bool
	}{
		{
			name:     "valid 64-char hex",
			input:    hexStr,
			wantAddr: knownAddress,
		},
		{
			name:     "valid base64",
			input:    b64Str,
			wantAddr: knownAddress,
		},
		{
			name:    "invalid hex – non-hex characters",
			input:   "zz" + hexStr[2:],
			wantErr: true,
		},
		{
			name:    "invalid base64",
			input:   "not-valid-base64!!",
			wantErr: true,
		},
		{
			name:    "base64 decodes to wrong length",
			input:   wrongLenB64,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeContractAddress(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Type != tc.wantAddr.Type {
				t.Fatalf("type mismatch: got %v, want %v", got.Type, tc.wantAddr.Type)
			}
			if got.ContractId == nil {
				t.Fatal("ContractId is nil")
			}
			if *got.ContractId != *tc.wantAddr.ContractId {
				t.Fatalf("ContractId mismatch:\n  got  %x\n  want %x", *got.ContractId, *tc.wantAddr.ContractId)
			}
		})
	}
}
