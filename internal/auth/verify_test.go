package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decredEcdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestNormalizeWalletType(t *testing.T) {
	tests := []struct {
		input string
		want  WalletType
		ok    bool
	}{
		{input: " EVM ", want: WalletTypeEVM, ok: true},
		{input: "stellar_ed25519", want: WalletTypeStellarEd25519, ok: true},
		{input: "Stellar_Secp256k1", want: WalletTypeStellarSecp256k1, ok: true},
		{input: "bitcoin", ok: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			got, err := NormalizeWalletType(tt.input)
			if tt.ok && err != nil {
				t.Fatalf("NormalizeWalletType returned error: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("NormalizeWalletType returned nil error")
			}
			if tt.ok && got != tt.want {
				t.Fatalf("wallet type = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeAddress(t *testing.T) {
	got, err := NormalizeAddress(WalletTypeEVM, "ABCDEFabcdef1234567890123456789012345678")
	if err != nil {
		t.Fatalf("NormalizeAddress returned error: %v", err)
	}
	if got != "0xabcdefabcdef1234567890123456789012345678" {
		t.Fatalf("normalized EVM address = %q", got)
	}

	if _, err := NormalizeAddress(WalletTypeEVM, "0x123"); err == nil {
		t.Fatal("short EVM address returned nil error")
	}
	if _, err := NormalizeAddress(WalletTypeEVM, " "); err == nil {
		t.Fatal("empty address returned nil error")
	}

	stellar, err := NormalizeAddress(WalletTypeStellarEd25519, " ABCDEF ")
	if err != nil {
		t.Fatalf("NormalizeAddress stellar returned error: %v", err)
	}
	if stellar != "abcdef" {
		t.Fatalf("normalized stellar address = %q, want abcdef", stellar)
	}

	if _, err := NormalizeAddress(WalletType("bitcoin"), "abc"); err == nil {
		t.Fatal("unsupported wallet type returned nil error")
	}
}

func TestVerifyEVMSignature(t *testing.T) {
	// precomputed EVM vector
	address := "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
	message := LoginMessage("nonce-123")
	// The signature below is precomputed for the above private key and message
	sigHex := "0x58325d3e95c9c1f438abb319ffee7a22d709709048e388660366fc2ebd92de0c4200cb340965857aec541822b168be361e0b77925d9c4f245fb0dc4adabf86bc00"
    signature, _ := hexutil.Decode(sigHex)
	// message and address are already set.
	// We don't need hash or sign anymore.

	if err := VerifySignature(WalletTypeEVM, address, message, hexutil.Encode(signature), ""); err != nil {
		t.Fatalf("VerifySignature EVM returned error: %v", err)
	}

	signatureWithLegacyV := append([]byte(nil), signature...)
	signatureWithLegacyV[64] += 27
	if err := VerifySignature(WalletTypeEVM, strings.ToLower(address), message, hexutil.Encode(signatureWithLegacyV), ""); err != nil {
		t.Fatalf("VerifySignature EVM with 27/28 V returned error: %v", err)
	}

	if err := VerifySignature(WalletTypeEVM, "0x0000000000000000000000000000000000000000", message, hexutil.Encode(signature), ""); err == nil {
		t.Fatal("VerifySignature accepted wrong EVM address")
	}
	if err := VerifySignature(WalletTypeEVM, address, message, "0x1234", ""); err == nil {
		t.Fatal("VerifySignature accepted short EVM signature")
	}
}

func TestVerifyStellarEd25519Signature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	message := LoginMessage("nonce-456")
	signature := ed25519.Sign(priv, []byte(message))

	if err := VerifySignature(WalletTypeStellarEd25519, "", message, hex.EncodeToString(signature), hex.EncodeToString(pub)); err != nil {
		t.Fatalf("VerifySignature ed25519 returned error: %v", err)
	}
	if err := VerifySignature(WalletTypeStellarEd25519, "", message+"x", hex.EncodeToString(signature), hex.EncodeToString(pub)); err == nil {
		t.Fatal("VerifySignature accepted ed25519 signature for wrong message")
	}
	if err := VerifySignature(WalletTypeStellarEd25519, "", message, "abcd", hex.EncodeToString(pub)); err == nil {
		t.Fatal("VerifySignature accepted malformed ed25519 signature")
	}
	if err := VerifySignature(WalletTypeStellarEd25519, "", message, hex.EncodeToString(signature), "abcd"); err == nil {
		t.Fatal("VerifySignature accepted malformed ed25519 public key")
	}
}

func TestVerifyStellarSecp256k1Signature(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey returned error: %v", err)
	}
	message := LoginMessage("nonce-789")
	hash := sha256.Sum256([]byte(message))
	signature := decredEcdsa.Sign(priv, hash[:])

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		"",
		message,
		hex.EncodeToString(signature.Serialize()),
		hex.EncodeToString(priv.PubKey().SerializeCompressed()),
	); err != nil {
		t.Fatalf("VerifySignature secp256k1 returned error: %v", err)
	}

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		"",
		message+"x",
		hex.EncodeToString(signature.Serialize()),
		hex.EncodeToString(priv.PubKey().SerializeCompressed()),
	); err == nil {
		t.Fatal("VerifySignature accepted secp256k1 signature for wrong message")
	}
	if err := VerifySignature(WalletTypeStellarSecp256k1, "", message, "abcd", hex.EncodeToString(priv.PubKey().SerializeCompressed())); err == nil {
		t.Fatal("VerifySignature accepted malformed secp256k1 signature")
	}
	if err := VerifySignature(WalletTypeStellarSecp256k1, "", message, hex.EncodeToString(signature.Serialize()), "abcd"); err == nil {
		t.Fatal("VerifySignature accepted malformed secp256k1 public key")
	}
}

func TestVerifyStellarSecp256k1CompactSignature(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey returned error: %v", err)
	}
	message := LoginMessage("nonce-compact")
	hash := sha256.Sum256([]byte(message))
	signature := decredEcdsa.Sign(priv, hash[:])

	r := signature.R()
	s := signature.S()
	compact := make([]byte, 64)
	r.PutBytesUnchecked(compact[:32])
	s.PutBytesUnchecked(compact[32:])

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		"",
		message,
		hex.EncodeToString(compact),
		hex.EncodeToString(priv.PubKey().SerializeCompressed()),
	); err != nil {
		t.Fatalf("VerifySignature compact secp256k1 returned error: %v", err)
	}
}

func TestVerifySignatureRejectsUnsupportedWalletType(t *testing.T) {
	if err := VerifySignature(WalletType("bitcoin"), "", "message", "abcd", "abcd"); err == nil {
		t.Fatal("VerifySignature accepted unsupported wallet type")
	}
}

func TestDecodeHex(t *testing.T) {
	got, err := decodeHex(" 0x0a ")
	if err != nil {
		t.Fatalf("decodeHex returned error: %v", err)
	}
	if len(got) != 1 || got[0] != 0x0a {
		t.Fatalf("decodeHex = %x, want 0a", got)
	}
	if _, err := decodeHex(" "); err == nil {
		t.Fatal("decodeHex accepted empty input")
	}
}

func TestParseSecp256k1SignatureRejectsInvalidCompactScalars(t *testing.T) {
	invalidR := make([]byte, 64)
	for i := 0; i < 32; i++ {
		invalidR[i] = 0xff
	}
	if _, err := parseSecp256k1Signature(invalidR); err == nil {
		t.Fatal("parseSecp256k1Signature accepted overflowing r scalar")
	}

	invalidS := make([]byte, 64)
	invalidS[31] = 1
	for i := 32; i < 64; i++ {
		invalidS[i] = 0xff
	}
	if _, err := parseSecp256k1Signature(invalidS); err == nil {
		t.Fatal("parseSecp256k1Signature accepted overflowing s scalar")
	}

	if _, err := parseSecp256k1Signature([]byte{0x30}); err == nil {
		t.Fatal("parseSecp256k1Signature accepted malformed DER")
	}
}
