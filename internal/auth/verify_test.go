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
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/stellar/go/strkey"
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
	privateKey, err := ethCrypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("HexToECDSA returned error: %v", err)
	}
	address := ethCrypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	message := LoginMessage("nonce-123")
	signature, err := ethCrypto.Sign(accounts.TextHash([]byte(message)), privateKey)
	if err != nil {
		t.Fatalf("Sign returned error: %v", err)
	}

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

	pubHex := hex.EncodeToString(pub)

	if err := VerifySignature(WalletTypeStellarEd25519, pubHex, message, hex.EncodeToString(signature), pubHex); err != nil {
		t.Fatalf("VerifySignature ed25519 returned error: %v", err)
	}
	if err := VerifySignature(WalletTypeStellarEd25519, pubHex, message+"x", hex.EncodeToString(signature), pubHex); err == nil {
		t.Fatal("VerifySignature accepted ed25519 signature for wrong message")
	}
	if err := VerifySignature(WalletTypeStellarEd25519, pubHex, message, "abcd", pubHex); err == nil {
		t.Fatal("VerifySignature accepted malformed ed25519 signature")
	}
	if err := VerifySignature(WalletTypeStellarEd25519, pubHex, message, hex.EncodeToString(signature), "abcd"); err == nil {
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
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		pubHex,
		message,
		hex.EncodeToString(signature.Serialize()),
		pubHex,
	); err != nil {
		t.Fatalf("VerifySignature secp256k1 returned error: %v", err)
	}

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		pubHex,
		message+"x",
		hex.EncodeToString(signature.Serialize()),
		pubHex,
	); err == nil {
		t.Fatal("VerifySignature accepted secp256k1 signature for wrong message")
	}
	if err := VerifySignature(WalletTypeStellarSecp256k1, pubHex, message, "abcd", pubHex); err == nil {
		t.Fatal("VerifySignature accepted malformed secp256k1 signature")
	}
	if err := VerifySignature(WalletTypeStellarSecp256k1, pubHex, message, hex.EncodeToString(signature.Serialize()), "abcd"); err == nil {
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
	pubHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	if err := VerifySignature(
		WalletTypeStellarSecp256k1,
		pubHex,
		message,
		hex.EncodeToString(compact),
		pubHex,
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

func TestVerifySignature_EdgeCases_EVM(t *testing.T) {
	keyA, err := ethCrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey A failed: %v", err)
	}
	keyB, err := ethCrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey B failed: %v", err)
	}

	addrA := ethCrypto.PubkeyToAddress(keyA.PublicKey).Hex() // EIP-55 Checksummed
	addrB := ethCrypto.PubkeyToAddress(keyB.PublicKey).Hex()

	msgA := LoginMessage("nonce-evm-A")
	msgB := LoginMessage("nonce-evm-B")

	sigA, err := ethCrypto.Sign(accounts.TextHash([]byte(msgA)), keyA)
	if err != nil {
		t.Fatalf("Sign A failed: %v", err)
	}
	sigAHex := hexutil.Encode(sigA)

	// 1. Valid signature over a different message payload (must be rejected)
	err = VerifySignature(WalletTypeEVM, addrA, msgB, sigAHex, "")
	if err == nil {
		t.Error("expected rejection for signature over a different message payload")
	} else if err.Error() != "signature does not match address" {
		t.Errorf("expected 'signature does not match address', got %q", err.Error())
	}

	// 2. Signature from a different key (must be rejected)
	err = VerifySignature(WalletTypeEVM, addrB, msgA, sigAHex, "")
	if err == nil {
		t.Error("expected rejection for signature from a different key")
	}

	// 3. Address case mismatch vs different key (checksum, lowercase, uppercase)
	if err := VerifySignature(WalletTypeEVM, addrA, msgA, sigAHex, ""); err != nil {
		t.Errorf("expected checksummed addrA to be accepted: %v", err)
	}
	if err := VerifySignature(WalletTypeEVM, strings.ToLower(addrA), msgA, sigAHex, ""); err != nil {
		t.Errorf("expected lowercase addrA to be accepted: %v", err)
	}
	if err := VerifySignature(WalletTypeEVM, strings.ToLower(addrB), msgA, sigAHex, ""); err == nil {
		t.Error("expected lowercase different address to be rejected")
	}
	if err := VerifySignature(WalletTypeEVM, strings.ToUpper(addrB), msgA, sigAHex, ""); err == nil {
		t.Error("expected uppercase different address to be rejected")
	}

	// 4. Truncated signature byte string
	truncatedSig := sigA[:64] // 64 bytes instead of 65
	err = VerifySignature(WalletTypeEVM, addrA, msgA, hexutil.Encode(truncatedSig), "")
	if err == nil || err.Error() != "invalid signature length" {
		t.Errorf("expected 'invalid signature length' for truncated sig, got %v", err)
	}

	// 5. Empty signature
	err = VerifySignature(WalletTypeEVM, addrA, msgA, "", "")
	if err == nil || err.Error() != "invalid signature hex" {
		t.Errorf("expected 'invalid signature hex' for empty sig, got %v", err)
	}

	// 6. Malformed hex signature
	err = VerifySignature(WalletTypeEVM, addrA, msgA, "0xzzzz", "")
	if err == nil || err.Error() != "invalid signature hex" {
		t.Errorf("expected 'invalid signature hex' for malformed sig, got %v", err)
	}

	// 7. Invalid recovery byte V (covers crypto.SigToPub failure branch in verifyEVM)
	invalidVSig := append([]byte(nil), sigA...)
	invalidVSig[64] = 5 // invalid V byte
	err = VerifySignature(WalletTypeEVM, addrA, msgA, hexutil.Encode(invalidVSig), "")
	if err == nil || err.Error() != "signature recovery failed" {
		t.Errorf("expected 'signature recovery failed', got %v", err)
	}
}

func TestVerifySignature_EdgeCases_StellarEd25519(t *testing.T) {
	pubA, privA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey A failed: %v", err)
	}
	pubB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey B failed: %v", err)
	}

	msgA := LoginMessage("nonce-ed-A")
	msgB := LoginMessage("nonce-ed-B")

	sigA := ed25519.Sign(privA, []byte(msgA))
	sigAHex := hex.EncodeToString(sigA)
	pubAHex := hex.EncodeToString(pubA)
	pubBHex := hex.EncodeToString(pubB)

	// 1. Valid signature over a different message payload
	err = VerifySignature(WalletTypeStellarEd25519, pubAHex, msgB, sigAHex, pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for message mismatch, got %v", err)
	}

	// 2. Signature from a different key (address matches the public_key
	// parameter, so this isolates a signature/key mismatch, not an
	// address/public_key mismatch)
	err = VerifySignature(WalletTypeStellarEd25519, pubBHex, msgA, sigAHex, pubBHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for different key, got %v", err)
	}

	// 3. Truncated signature byte string
	truncatedSigHex := hex.EncodeToString(sigA[:32]) // 32 bytes instead of 64
	err = VerifySignature(WalletTypeStellarEd25519, pubAHex, msgA, truncatedSigHex, pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for truncated sig, got %v", err)
	}

	// 4. Empty signature
	err = VerifySignature(WalletTypeStellarEd25519, pubAHex, msgA, "", pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for empty sig, got %v", err)
	}

	// 5. Invalid public key length (e.g. 31 bytes instead of 32)
	shortPubHex := hex.EncodeToString(pubA[:31])
	err = VerifySignature(WalletTypeStellarEd25519, pubAHex, msgA, sigAHex, shortPubHex)
	if err == nil || err.Error() != "invalid public_key" {
		t.Errorf("expected 'invalid public_key', got %v", err)
	}
}

// TestVerifySignature_StellarAddressBinding_Ed25519 covers Issue #316: address
// must cryptographically correspond to public_key, not just be an
// unauthenticated label the caller supplies.
func TestVerifySignature_StellarAddressBinding_Ed25519(t *testing.T) {
	pubA, privA, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey A failed: %v", err)
	}
	pubB, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey B failed: %v", err)
	}

	message := LoginMessage("nonce-addr-bind")
	sig := ed25519.Sign(privA, []byte(message))
	sigHex := hex.EncodeToString(sig)
	pubAHex := hex.EncodeToString(pubA)

	strkeyA, err := strkey.Encode(strkey.VersionByteAccountID, pubA)
	if err != nil {
		t.Fatalf("encode StrKey address: %v", err)
	}
	strkeyB, err := strkey.Encode(strkey.VersionByteAccountID, pubB)
	if err != nil {
		t.Fatalf("encode StrKey address: %v", err)
	}

	// A genuine signature from key A must NOT authenticate as key B's
	// address — the core exploit this issue describes.
	if err := VerifySignature(WalletTypeStellarEd25519, strkeyB, message, sigHex, pubAHex); err == nil {
		t.Fatal("VerifySignature accepted key A's signature for key B's StrKey address")
	}

	// The correct StrKey address for the signing key must succeed.
	if err := VerifySignature(WalletTypeStellarEd25519, strkeyA, message, sigHex, pubAHex); err != nil {
		t.Fatalf("VerifySignature rejected the correct StrKey address: %v", err)
	}

	// The raw hex-encoded public key is also a documented address form and
	// must succeed (case-insensitive, since hex has no case meaning).
	if err := VerifySignature(WalletTypeStellarEd25519, strings.ToUpper(pubAHex), message, sigHex, pubAHex); err != nil {
		t.Fatalf("VerifySignature rejected the raw-hex address form: %v", err)
	}

	// An address matching neither the StrKey nor the hex form of the actual
	// key must be rejected outright.
	if err := VerifySignature(WalletTypeStellarEd25519, hex.EncodeToString(pubB), message, sigHex, pubAHex); err == nil {
		t.Fatal("VerifySignature accepted an address matching a different key's hex form")
	}
}

func TestVerifySignature_EdgeCases_StellarSecp256k1(t *testing.T) {
	privA, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey A failed: %v", err)
	}
	privB, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey B failed: %v", err)
	}

	msgA := LoginMessage("nonce-secp-A")
	msgB := LoginMessage("nonce-secp-B")

	hashA := sha256.Sum256([]byte(msgA))
	sigA := decredEcdsa.Sign(privA, hashA[:])
	sigAHex := hex.EncodeToString(sigA.Serialize())
	pubAHex := hex.EncodeToString(privA.PubKey().SerializeCompressed())
	pubBHex := hex.EncodeToString(privB.PubKey().SerializeCompressed())

	// 1. Valid signature over a different message payload
	err = VerifySignature(WalletTypeStellarSecp256k1, pubAHex, msgB, sigAHex, pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for message mismatch, got %v", err)
	}

	// 2. Signature from a different key (address matches the public_key
	// parameter, so this isolates a signature/key mismatch)
	err = VerifySignature(WalletTypeStellarSecp256k1, pubBHex, msgA, sigAHex, pubBHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for different key, got %v", err)
	}

	// 3. Truncated signature byte string
	truncatedSigHex := hex.EncodeToString(sigA.Serialize()[:30])
	err = VerifySignature(WalletTypeStellarSecp256k1, pubAHex, msgA, truncatedSigHex, pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for truncated sig, got %v", err)
	}

	// 4. Empty signature
	err = VerifySignature(WalletTypeStellarSecp256k1, pubAHex, msgA, "", pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for empty sig, got %v", err)
	}

	// 5. Invalid public key bytes (valid hex, but wrong length/format for secp256k1, covers secp256k1ParsePubKey error branch)
	invalidPubHex := "0102030405060708090a"
	err = VerifySignature(WalletTypeStellarSecp256k1, "", msgA, sigAHex, invalidPubHex)
	if err == nil || err.Error() != "invalid public_key" {
		t.Errorf("expected 'invalid public_key', got %v", err)
	}

	// 6. Invalid signature bytes (valid hex, but wrong DER/compact format, covers parseSecp256k1Signature error branch)
	invalidSigHex := "0102030405060708090a"
	err = VerifySignature(WalletTypeStellarSecp256k1, pubAHex, msgA, invalidSigHex, pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature', got %v", err)
	}

	// 7. Invalid hex character in signature and public key (covers decodeHex error branch in verifyStellarSecp256k1)
	err = VerifySignature(WalletTypeStellarSecp256k1, pubAHex, msgA, "not-a-hex-string", pubAHex)
	if err == nil || err.Error() != "invalid signature" {
		t.Errorf("expected 'invalid signature' for non-hex signature, got %v", err)
	}
	err = VerifySignature(WalletTypeStellarSecp256k1, "", msgA, sigAHex, "not-a-hex-string")
	if err == nil || err.Error() != "invalid public_key" {
		t.Errorf("expected 'invalid public_key' for non-hex public key, got %v", err)
	}
}

// TestVerifySignature_StellarAddressBinding_Secp256k1 mirrors the Ed25519
// address-binding coverage above (Issue #316). Stellar has no StrKey
// encoding for secp256k1 keys, so the only documented address form here is
// the raw hex public key.
func TestVerifySignature_StellarAddressBinding_Secp256k1(t *testing.T) {
	privA, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey A failed: %v", err)
	}
	privB, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey B failed: %v", err)
	}

	message := LoginMessage("nonce-secp-addr-bind")
	hash := sha256.Sum256([]byte(message))
	sig := decredEcdsa.Sign(privA, hash[:])
	sigHex := hex.EncodeToString(sig.Serialize())
	pubAHex := hex.EncodeToString(privA.PubKey().SerializeCompressed())
	pubBHex := hex.EncodeToString(privB.PubKey().SerializeCompressed())

	// A genuine signature from key A must not authenticate as key B's address.
	if err := VerifySignature(WalletTypeStellarSecp256k1, pubBHex, message, sigHex, pubAHex); err == nil {
		t.Fatal("VerifySignature accepted key A's signature for key B's address")
	}

	// The correct hex address (any case) for the signing key must succeed.
	if err := VerifySignature(WalletTypeStellarSecp256k1, strings.ToUpper(pubAHex), message, sigHex, pubAHex); err != nil {
		t.Fatalf("VerifySignature rejected the correct address: %v", err)
	}
}

// TestNormalizeAddress_PreservesStrKeyCase covers Issue #316's suggested
// execution point 5: StrKey addresses are case-sensitive and checksummed,
// so lowercasing one (as NormalizeAddress previously did unconditionally)
// would make it permanently fail the address/public-key binding check in
// VerifySignature. Only the non-StrKey (raw hex) form should be lowercased.
func TestNormalizeAddress_PreservesStrKeyCase(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	strkeyAddr, err := strkey.Encode(strkey.VersionByteAccountID, pub)
	if err != nil {
		t.Fatalf("encode StrKey address: %v", err)
	}

	got, err := NormalizeAddress(WalletTypeStellarEd25519, " "+strkeyAddr+" ")
	if err != nil {
		t.Fatalf("NormalizeAddress returned error: %v", err)
	}
	if got != strkeyAddr {
		t.Fatalf("NormalizeAddress altered a StrKey address: got %q, want %q", got, strkeyAddr)
	}

	// Raw hex addresses are still lowercased as before.
	got, err = NormalizeAddress(WalletTypeStellarEd25519, " ABCDEF ")
	if err != nil {
		t.Fatalf("NormalizeAddress returned error: %v", err)
	}
	if got != "abcdef" {
		t.Fatalf("normalized hex address = %q, want abcdef", got)
	}
}
