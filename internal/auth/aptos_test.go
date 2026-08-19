package auth

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestAptosFullMessage_MatchesTheWalletEnvelope(t *testing.T) {
	got := AptosFullMessage("hello", "abc123")
	want := "APTOS\nmessage: hello\nnonce: abc123"
	if got != want {
		t.Fatalf("envelope\n got  %q\n want %q", got, want)
	}
	// A signature is over the ENVELOPE, never the bare message. If these were
	// ever equal, the distinction this function exists for would be gone.
	if got == "hello" {
		t.Fatal("the envelope is the bare message")
	}
}

func TestVerifyAptosSignature_RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	addr := AptosAddressFromEd25519(pub)
	msg, nonce := "Grainlify payout address verification", "n-1"
	sig := ed25519.Sign(priv, []byte(AptosFullMessage(msg, nonce)))

	derived, err := VerifyAptosSignature(addr, msg, nonce, hex.EncodeToString(sig), hex.EncodeToString(pub))
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if derived != addr {
		t.Fatalf("derived %s want %s", derived, addr)
	}
}

// Signing the BARE message must not verify. This is the failure that looks like
// a wallet bug: a perfectly valid signature over the wrong bytes.
func TestVerifyAptosSignature_BareMessageDoesNotVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	addr := AptosAddressFromEd25519(pub)
	sig := ed25519.Sign(priv, []byte("m"))
	if _, err := VerifyAptosSignature(addr, "m", "n", hex.EncodeToString(sig), hex.EncodeToString(pub)); !errors.Is(err, ErrAptosBadSig) {
		t.Fatalf("a signature over the bare message verified: %v", err)
	}
}

// A signature made for one nonce must not verify for another, or a captured
// signature is replayable forever.
func TestVerifyAptosSignature_NonceIsBoundIn(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	addr := AptosAddressFromEd25519(pub)
	sig := ed25519.Sign(priv, []byte(AptosFullMessage("m", "nonce-A")))
	if _, err := VerifyAptosSignature(addr, "m", "nonce-B", hex.EncodeToString(sig), hex.EncodeToString(pub)); !errors.Is(err, ErrAptosBadSig) {
		t.Fatalf("a signature for another nonce verified: %v", err)
	}
}

// The whole point of signature_address_mismatch: a valid signature from the
// wrong key must be refused, and the derived address reported.
func TestVerifyAptosSignature_WrongKeyIsAMismatchNotAnAcceptance(t *testing.T) {
	_, privA, _ := ed25519.GenerateKey(nil)
	pubB, _, _ := ed25519.GenerateKey(nil)
	claimed := AptosAddressFromEd25519(pubB)

	pubA := privA.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(privA, []byte(AptosFullMessage("m", "n")))

	derived, err := VerifyAptosSignature(claimed, "m", "n", hex.EncodeToString(sig), hex.EncodeToString(pubA))
	if !errors.Is(err, ErrAptosAddrMismatch) {
		t.Fatalf("want ErrAptosAddrMismatch, got %v", err)
	}
	if derived == claimed {
		t.Fatal("derived address equals the claimed one on a mismatch")
	}
	if derived != AptosAddressFromEd25519(pubA) {
		t.Fatal("the reported derived address is not the signer's")
	}
}

func TestAptosAddress_TwoSchemesGiveDifferentAddresses(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	if AptosAddressFromEd25519(pub) == AptosAddressFromSingleKey(pub) {
		t.Fatal("Ed25519 and SingleKey derivations collide; trying both would be pointless")
	}
}

// A SingleKey account must verify too, or Aptos Connect users are refused.
func TestVerifyAptosSignature_AcceptsASingleKeyAddress(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	addr := AptosAddressFromSingleKey(pub)
	sig := ed25519.Sign(priv, []byte(AptosFullMessage("m", "n")))
	derived, err := VerifyAptosSignature(addr, "m", "n", hex.EncodeToString(sig), hex.EncodeToString(pub))
	if err != nil {
		t.Fatalf("a SingleKey account was refused: %v", err)
	}
	if derived != addr {
		t.Fatalf("derived %s want %s", derived, addr)
	}
}

func TestAptosAddress_IsCanonicalLength(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	for _, a := range []string{AptosAddressFromEd25519(pub), AptosAddressFromSingleKey(pub)} {
		if len(a) != 66 || !strings.HasPrefix(a, "0x") {
			t.Errorf("derived address %q is not 0x + 64 hex", a)
		}
	}
}

func TestVerifyAptosSignature_RejectsMalformedInputs(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	addr := AptosAddressFromEd25519(pub)
	good := hex.EncodeToString(ed25519.Sign(priv, []byte(AptosFullMessage("m", "n"))))
	for _, tc := range []struct {
		sig, key string
		want     error
	}{
		{good, "00", ErrAptosBadPublicKey},
		{"00", hex.EncodeToString(pub), ErrAptosBadSignature},
		{"zz", hex.EncodeToString(pub), ErrAptosBadSignature},
	} {
		if _, err := VerifyAptosSignature(addr, "m", "n", tc.sig, tc.key); !errors.Is(err, tc.want) {
			t.Errorf("sig=%.4s key=%.4s: want %v, got %v", tc.sig, tc.key, tc.want, err)
		}
	}
}
