package sponsor

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

// The sponsor's address must be derived from its key, never configured
// alongside it: an address that drifts from the key it names produces a
// signature the node attributes to an account that did not sign.
func TestAptosAddress_AgreesWithTheVerifiedDerivation(t *testing.T) {
	// A key we control, to prove the ALGORITHM: sha3-256(pubkey || 0x00).
	//
	// The sponsor's private key is not in this repository, so this cannot derive
	// the real 0x1b41…22c9 directly. What it does instead is agree with
	// internal/auth's derivation, which IS pinned against a real Petra account by
	// the wallet check - so the chain of evidence ends outside this repository
	// even though this assertion does not reach it directly. Stated rather than
	// implied, because "derives the real address" would overclaim.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	s := &Signer{priv: priv}

	got := s.AptosAddress()
	if !strings.HasPrefix(got, "0x") || len(got) != 66 {
		t.Fatalf("address %q is not 0x + 64 hex", got)
	}
	// Cross-checked against internal/auth, which derives the same way for
	// contributor addresses and is itself pinned by the wallet check against a
	// real Petra account.
	pub := priv.Public().(ed25519.PublicKey)
	if want := aptosAddrViaAuth(pub); got != want {
		t.Fatalf("derivation disagrees with internal/auth:\n  sponsor %s\n  auth    %s", got, want)
	}
}

// The fee payer signs a DIFFERENT message from the sender when the sender used
// the zero-address form. Reusing the sender's message would sign the wrong bytes.
func TestSubmit_FeePayerSignsItsOwnAddressNotZero(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		w.Write([]byte(`{"hash":"0xabc"}`))
	}))
	defer srv.Close()

	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 3
	signer := &Signer{priv: ed25519.NewKeyFromSeed(seed)}
	sub := NewSubmitter(srv.URL, signer)

	f := milestoneFields() // sender signed the ZERO form
	senderPub := unhex(t, milestone1.PubKey)
	senderSig := unhex(t, milestone1.Signature)

	hash, err := sub.SubmitSponsored(context.Background(), f, senderPub, senderSig)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "0xabc" {
		t.Fatalf("hash = %q", hash)
	}

	sig := body["signature"].(map[string]any)
	if sig["fee_payer_address"] == ZeroAddress {
		t.Fatal("submitted with fee_payer_address 0x0; the payer is known by now and must be named")
	}
	if sig["fee_payer_address"] != signer.AptosAddress() {
		t.Errorf("fee_payer_address = %v, want the signer's own address", sig["fee_payer_address"])
	}
	// And the payer's signature must verify over ITS OWN message, not the
	// sender's.
	payerFields := f
	payerFields.FeePayer = signer.AptosAddress()
	msg, _ := FeePayerSigningMessage(payerFields)
	fps := sig["fee_payer_signer"].(map[string]any)
	pk := unhex(t, fps["public_key"].(string))
	ps := unhex(t, fps["signature"].(string))
	if !ed25519.Verify(pk, msg, ps) {
		t.Fatal("the fee payer's signature does not verify over the named-payer message")
	}
}

// A rejected submission must keep the node's own explanation.
func TestSubmit_KeepsTheUpstreamStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"message":"INVALID_SIGNATURE"}`))
	}))
	defer srv.Close()
	seed := make([]byte, ed25519.SeedSize)
	sub := NewSubmitter(srv.URL, &Signer{priv: ed25519.NewKeyFromSeed(seed)})
	_, err := sub.SubmitSponsored(context.Background(), milestoneFields(), make([]byte, 32), make([]byte, 64))
	if !errors.Is(err, ErrSubmitFailed) {
		t.Fatalf("want ErrSubmitFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "INVALID_SIGNATURE") {
		t.Errorf("the node's explanation was discarded: %v", err)
	}
}

// A 200 with no hash is not a success.
func TestSubmit_RefusesAReplyWithNoHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	seed := make([]byte, ed25519.SeedSize)
	sub := NewSubmitter(srv.URL, &Signer{priv: ed25519.NewKeyFromSeed(seed)})
	if _, err := sub.SubmitSponsored(context.Background(), milestoneFields(), make([]byte, 32), make([]byte, 64)); err == nil {
		t.Fatal("a reply with no transaction hash was treated as a successful submission")
	}
}

func aptosAddrViaAuth(pub ed25519.PublicKey) string {
	// Mirrors internal/auth.AptosAddressFromEd25519 without importing it, to keep
	// this package free of a dependency it does not otherwise need.
	h := sha3.New256()
	h.Write(pub)
	h.Write([]byte{0x00})
	return "0x" + hex.EncodeToString(h.Sum(nil))
}
