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
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
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
		t.Run(tt.input, func(t *testing.T) {
			got, err := NormalizeWalletType(tt.input)
			if tt.ok && err != nil {
				t.Fatalf("NormalizeWalletType returned error: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("NormalizeWalletType returned nil error")
			}
			if got != tt.want {
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
}

func TestVerifyEVMSignature(t *testing.T) {
	key, err := ethcrypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	message := LoginMessage("nonce-123")
	hash := accounts.TextHash([]byte(message))
	signature, err := ethcrypto.Sign(hash, key)
	if err != nil {
		t.Fatalf("Sign returned error: %v", err)
	}
	address := ethcrypto.PubkeyToAddress(key.PublicKey).Hex()

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
