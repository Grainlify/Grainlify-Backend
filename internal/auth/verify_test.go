package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// vsigNewEVMSigner generates a fresh secp256k1/EVM keypair and returns its
// checksummed-lowercase address plus a sign func that produces a raw
// 65-byte [R||S||V] signature (V in {0,1}) over an arbitrary digest, exactly
// as github.com/ethereum/go-ethereum/crypto.Sign does.
func vsigNewEVMSigner(t *testing.T) (address string, sign func(hash []byte) []byte) {
	t.Helper()
	priv, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("crypto.GenerateKey: %v", err)
	}
	address = strings.ToLower(crypto.PubkeyToAddress(priv.PublicKey).Hex())
	sign = func(hash []byte) []byte {
		sig, err := crypto.Sign(hash, priv)
		if err != nil {
			t.Fatalf("crypto.Sign: %v", err)
		}
		return sig
	}
	return address, sign
}

// vsigNewEd25519Signer generates a fresh ed25519 keypair (as used by
// WalletTypeStellarEd25519) and returns its hex-encoded public key plus a
// sign func that hex-encodes an ed25519 signature over the raw message.
func vsigNewEd25519Signer(t *testing.T) (publicKeyHex string, sign func(message string) string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	publicKeyHex = hex.EncodeToString(pub)
	sign = func(message string) string {
		return hex.EncodeToString(ed25519.Sign(priv, []byte(message)))
	}
	return publicKeyHex, sign
}

// vsigNewSecp256k1Signer generates a fresh secp256k1 keypair (as used by
// WalletTypeStellarSecp256k1) and returns its hex-encoded compressed public
// key plus a sign func that DER-encodes an ECDSA signature over
// SHA-256(message), matching verifyStellarSecp256k1's expectations exactly.
func vsigNewSecp256k1Signer(t *testing.T) (publicKeyHex string, sign func(message string) string) {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("secp256k1.GeneratePrivateKey: %v", err)
	}
	publicKeyHex = hex.EncodeToString(priv.PubKey().SerializeCompressed())
	sign = func(message string) string {
		h := sha256.Sum256([]byte(message))
		sig := ecdsa.Sign(priv, h[:])
		return hex.EncodeToString(sig.Serialize())
	}
	return publicKeyHex, sign
}

// --- EVM ---

func TestVerifySignature_EVM_Valid(t *testing.T) {
	address, sign := vsigNewEVMSigner(t)
	message := "Patchwork login. Nonce: test-nonce-evm"
	sigHex := hexutil.Encode(sign(accounts.TextHash([]byte(message))))

	if err := VerifySignature(WalletTypeEVM, address, message, sigHex, ""); err != nil {
		t.Fatalf("VerifySignature: unexpected error: %v", err)
	}
}

func TestVerifySignature_EVM_ValidWithLegacyVOffset(t *testing.T) {
	// Some signing tools return the recovery id (V) as 27/28 instead of
	// 0/1; verifyEVM must normalize this before recovering the pubkey.
	address, sign := vsigNewEVMSigner(t)
	message := "Patchwork login. Nonce: test-nonce-evm-legacy-v"
	sig := sign(accounts.TextHash([]byte(message)))
	sig[64] += 27 // 0/1 -> 27/28
	sigHex := hexutil.Encode(sig)

	if err := VerifySignature(WalletTypeEVM, address, message, sigHex, ""); err != nil {
		t.Fatalf("VerifySignature with legacy V offset: unexpected error: %v", err)
	}
}

func TestVerifySignature_EVM_TamperedMessage(t *testing.T) {
	address, sign := vsigNewEVMSigner(t)
	message := "Patchwork login. Nonce: original"
	sigHex := hexutil.Encode(sign(accounts.TextHash([]byte(message))))

	if err := VerifySignature(WalletTypeEVM, address, "Patchwork login. Nonce: tampered", sigHex, ""); err == nil {
		t.Fatal("VerifySignature: expected error for tampered message, got nil")
	}
}

func TestVerifySignature_EVM_WrongSignerAddress(t *testing.T) {
	_, sign := vsigNewEVMSigner(t)
	otherAddress, _ := vsigNewEVMSigner(t)
	message := "Patchwork login. Nonce: addr-mismatch"
	sigHex := hexutil.Encode(sign(accounts.TextHash([]byte(message))))

	if err := VerifySignature(WalletTypeEVM, otherAddress, message, sigHex, ""); err == nil {
		t.Fatal("VerifySignature: expected error for wrong signer address, got nil")
	}
}

func TestVerifySignature_EVM_MalformedSignature(t *testing.T) {
	address, _ := vsigNewEVMSigner(t)
	cases := []struct {
		name string
		sig  string
	}{
		{"not hex / no 0x prefix", "not-a-hex-signature"},
		{"valid hex but too short", "0x1234"},
		{"empty string", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifySignature(WalletTypeEVM, address, "any message", tc.sig, ""); err == nil {
				t.Fatal("VerifySignature: expected error for malformed signature, got nil")
			}
		})
	}
}

// --- Stellar ed25519 ---

func TestVerifySignature_StellarEd25519_Valid(t *testing.T) {
	pubKeyHex, sign := vsigNewEd25519Signer(t)
	message := "Patchwork login. Nonce: stellar-ed25519"
	sigHex := sign(message)

	if err := VerifySignature(WalletTypeStellarEd25519, pubKeyHex, message, sigHex, pubKeyHex); err != nil {
		t.Fatalf("VerifySignature: unexpected error: %v", err)
	}
}

func TestVerifySignature_StellarEd25519_TamperedMessage(t *testing.T) {
	pubKeyHex, sign := vsigNewEd25519Signer(t)
	message := "Patchwork login. Nonce: original"
	sigHex := sign(message)

	if err := VerifySignature(WalletTypeStellarEd25519, pubKeyHex, "Patchwork login. Nonce: tampered", sigHex, pubKeyHex); err == nil {
		t.Fatal("expected error for tampered message, got nil")
	}
}

func TestVerifySignature_StellarEd25519_WrongPublicKey(t *testing.T) {
	_, sign := vsigNewEd25519Signer(t)
	otherPubKeyHex, _ := vsigNewEd25519Signer(t)
	message := "Patchwork login. Nonce: pubkey-mismatch"
	sigHex := sign(message)

	// address matches the publicKeyHex being tested, so this exercises the
	// signature/pubkey mismatch specifically, not the address/pubkey check.
	if err := VerifySignature(WalletTypeStellarEd25519, otherPubKeyHex, message, sigHex, otherPubKeyHex); err == nil {
		t.Fatal("expected error for wrong public key, got nil")
	}
}

func TestVerifySignature_StellarEd25519_MalformedSignature(t *testing.T) {
	pubKeyHex, _ := vsigNewEd25519Signer(t)
	if err := VerifySignature(WalletTypeStellarEd25519, pubKeyHex, "any message", "not-hex-sig", pubKeyHex); err == nil {
		t.Fatal("expected error for malformed signature, got nil")
	}
}

func TestVerifySignature_StellarEd25519_MalformedPublicKey(t *testing.T) {
	_, sign := vsigNewEd25519Signer(t)
	sigHex := sign("any message")
	if err := VerifySignature(WalletTypeStellarEd25519, "not-hex-pubkey", "any message", sigHex, "not-hex-pubkey"); err == nil {
		t.Fatal("expected error for malformed public key, got nil")
	}
}

// TestVerifySignature_StellarEd25519_AddressMustMatchPublicKey is a
// regression guard for a real vulnerability found while writing these
// tests and fixed in verify.go (addressMatchesPublicKey): the Stellar
// ed25519 branch used to verify only that signatureHex was valid under
// publicKeyHex, without ever checking that the caller-claimed `address`
// corresponded to that key. That let a caller sign with a key they control
// while claiming an unrelated address, and log in as that address. Unlike
// the EVM path (which cryptographically recovers and compares the address),
// Stellar addresses here are just the hex public key, so this must be
// checked explicitly - if this test ever passes again, the binding was
// removed.
func TestVerifySignature_StellarEd25519_AddressMustMatchPublicKey(t *testing.T) {
	pubKeyHex, sign := vsigNewEd25519Signer(t)
	message := "Patchwork login. Nonce: address-must-match"
	sigHex := sign(message)

	err := VerifySignature(WalletTypeStellarEd25519, "completely-unrelated-address", message, sigHex, pubKeyHex)
	if err == nil {
		t.Fatal("expected error: address does not correspond to the signing public key, got nil")
	}
}

// --- Stellar secp256k1 ---

func TestVerifySignature_StellarSecp256k1_Valid(t *testing.T) {
	pubKeyHex, sign := vsigNewSecp256k1Signer(t)
	message := "Patchwork login. Nonce: stellar-secp256k1"
	sigHex := sign(message)

	if err := VerifySignature(WalletTypeStellarSecp256k1, pubKeyHex, message, sigHex, pubKeyHex); err != nil {
		t.Fatalf("VerifySignature: unexpected error: %v", err)
	}
}

// TestVerifySignature_StellarSecp256k1_AddressMustMatchPublicKey mirrors
// TestVerifySignature_StellarEd25519_AddressMustMatchPublicKey - the same
// address/public-key binding gap existed in verifyStellarSecp256k1 and was
// fixed alongside the ed25519 branch.
func TestVerifySignature_StellarSecp256k1_AddressMustMatchPublicKey(t *testing.T) {
	pubKeyHex, sign := vsigNewSecp256k1Signer(t)
	message := "Patchwork login. Nonce: address-must-match"
	sigHex := sign(message)

	err := VerifySignature(WalletTypeStellarSecp256k1, "completely-unrelated-address", message, sigHex, pubKeyHex)
	if err == nil {
		t.Fatal("expected error: address does not correspond to the signing public key, got nil")
	}
}

func TestVerifySignature_StellarSecp256k1_CompactSignatureFormat(t *testing.T) {
	// verifyStellarSecp256k1 also accepts a 64-byte compact (R||S) signature
	// in addition to DER; exercise that branch explicitly.
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("secp256k1.GeneratePrivateKey: %v", err)
	}
	pubKeyHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())
	message := "Patchwork login. Nonce: compact-sig"
	h := sha256.Sum256([]byte(message))
	sig := ecdsa.Sign(priv, h[:])

	r := sig.R()
	s := sig.S()
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	compact := append(append([]byte{}, rBytes[:]...), sBytes[:]...)
	sigHex := hex.EncodeToString(compact)

	if err := VerifySignature(WalletTypeStellarSecp256k1, pubKeyHex, message, sigHex, pubKeyHex); err != nil {
		t.Fatalf("unexpected error verifying compact-form signature: %v", err)
	}
}

func TestVerifySignature_StellarSecp256k1_TamperedMessage(t *testing.T) {
	pubKeyHex, sign := vsigNewSecp256k1Signer(t)
	message := "Patchwork login. Nonce: original"
	sigHex := sign(message)

	if err := VerifySignature(WalletTypeStellarSecp256k1, pubKeyHex, "Patchwork login. Nonce: tampered", sigHex, pubKeyHex); err == nil {
		t.Fatal("expected error for tampered message, got nil")
	}
}

func TestVerifySignature_StellarSecp256k1_WrongPublicKey(t *testing.T) {
	_, sign := vsigNewSecp256k1Signer(t)
	otherPubKeyHex, _ := vsigNewSecp256k1Signer(t)
	message := "Patchwork login. Nonce: pubkey-mismatch"
	sigHex := sign(message)

	// address matches the publicKeyHex being tested, so this exercises the
	// signature/pubkey mismatch specifically, not the address/pubkey check.
	if err := VerifySignature(WalletTypeStellarSecp256k1, otherPubKeyHex, message, sigHex, otherPubKeyHex); err == nil {
		t.Fatal("expected error for wrong public key, got nil")
	}
}

func TestVerifySignature_StellarSecp256k1_MalformedPublicKey(t *testing.T) {
	_, sign := vsigNewSecp256k1Signer(t)
	sigHex := sign("any message")

	cases := []struct {
		name      string
		publicKey string
	}{
		{"not hex", "not-hex-pubkey"},
		{"valid hex, wrong length for a pubkey", "aabbccdd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifySignature(WalletTypeStellarSecp256k1, tc.publicKey, "any message", sigHex, tc.publicKey); err == nil {
				t.Fatal("expected error for malformed public key, got nil")
			}
		})
	}
}

func TestVerifySignature_StellarSecp256k1_MalformedSignature(t *testing.T) {
	pubKeyHex, _ := vsigNewSecp256k1Signer(t)
	if err := VerifySignature(WalletTypeStellarSecp256k1, pubKeyHex, "any message", "not-hex-sig", pubKeyHex); err == nil {
		t.Fatal("expected error for malformed signature, got nil")
	}
}

// --- unsupported wallet type ---

func TestVerifySignature_UnsupportedWalletType(t *testing.T) {
	if err := VerifySignature(WalletType("bitcoin"), "addr", "message", "sig", ""); err == nil {
		t.Fatal("expected error for unsupported wallet type, got nil")
	}
}

// --- NormalizeWalletType / NormalizeAddress (also defined in verify.go) ---

func TestNormalizeWalletType(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    WalletType
		wantErr bool
	}{
		{"evm lowercase", "evm", WalletTypeEVM, false},
		{"evm uppercase", "EVM", WalletTypeEVM, false},
		{"evm with surrounding whitespace", "  evm  ", WalletTypeEVM, false},
		{"stellar ed25519", "stellar_ed25519", WalletTypeStellarEd25519, false},
		{"stellar secp256k1", "stellar_secp256k1", WalletTypeStellarSecp256k1, false},
		{"unsupported", "bitcoin", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeWalletType(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizeAddress(t *testing.T) {
	t.Run("evm without 0x prefix gets one added", func(t *testing.T) {
		addr := strings.Repeat("A", 40)
		got, err := NormalizeAddress(WalletTypeEVM, addr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "0x" + strings.Repeat("a", 40)
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("evm with 0x prefix normalized to lowercase", func(t *testing.T) {
		addr := "0x" + strings.Repeat("B", 40)
		got, err := NormalizeAddress(WalletTypeEVM, addr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "0x" + strings.Repeat("b", 40)
		if got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("evm wrong length rejected", func(t *testing.T) {
		if _, err := NormalizeAddress(WalletTypeEVM, "0x1234"); err == nil {
			t.Fatal("expected error for short address, got nil")
		}
	})

	t.Run("empty address rejected", func(t *testing.T) {
		if _, err := NormalizeAddress(WalletTypeEVM, "   "); err == nil {
			t.Fatal("expected error for empty address, got nil")
		}
	})

	t.Run("stellar address lowercased and trimmed", func(t *testing.T) {
		got, err := NormalizeAddress(WalletTypeStellarEd25519, "  ABCDEF  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "abcdef" {
			t.Errorf("got %v, want abcdef", got)
		}
	})
}
