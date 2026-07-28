package soroban

import (
	"testing"

	"github.com/stellar/go/xdr"
)

func TestContractAddress_String_WithContractId(t *testing.T) {
	var hash xdr.Hash
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	contractID := xdr.ContractId(hash)
	ca := &ContractAddress{
		ScAddress: xdr.ScAddress{
			Type:       xdr.ScAddressTypeScAddressTypeContract,
			ContractId: &contractID,
		},
	}

	got := ca.String()
	want := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestContractAddress_String_NilContractId(t *testing.T) {
	ca := &ContractAddress{ScAddress: xdr.ScAddress{}}

	if got := ca.String(); got != "" {
		t.Errorf("String() = %q, want empty string for a nil ContractId", got)
	}
}

// DefaultRetryConfig is already covered by TestDefaultRetryConfig in
// xdr_helpers_test.go.
