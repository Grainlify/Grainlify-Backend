package soroban

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stellar/go/xdr"
)

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
		Type:        xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &knownContractID,
	}

	hexStr := hex.EncodeToString(knownBytes[:])
	b64Str := base64.StdEncoding.EncodeToString(knownBytes[:])

	// 16 zero bytes in base64 – decodes successfully but is only 16 bytes.
	wrongLenBytes := make([]byte, 16)
	wrongLenB64 := base64.StdEncoding.EncodeToString(wrongLenBytes)

	tests := []struct {
		name      string
		input     string
		wantAddr  xdr.ScAddress
		wantErr   bool
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
