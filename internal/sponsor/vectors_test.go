package sponsor

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type feePayerVector struct {
	Name     string   `json:"name"`
	FeePayer string   `json:"feePayer"`
	Proof    []string `json:"proof"`
	Ident    string   `json:"ident"`
	Sender   string   `json:"sender"`
	Seq      uint64   `json:"seq"`
	MaxGas   uint64   `json:"maxGas"`
	Price    uint64   `json:"price"`
	Expires  uint64   `json:"expires"`
	Module   string   `json:"module"`
	Escrow   string   `json:"escrow"`
	Amount   string   `json:"amount"`
	Message  string   `json:"message"`
}

func loadVectors(t *testing.T) []feePayerVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/feepayer_vectors.jsonl")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var out []feePayerVector
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v feePayerVector
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("parse vector: %v", err)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		t.Fatal("no vectors loaded; an empty file finds no mismatches, which is " +
			"indistinguishable from a correct encoder")
	}
	return out
}

// THE PRODUCTION-SHAPE VECTORS.
//
// Milestone 1 was a single-leaf tree, so its proof array is EMPTY - a shape that
// will never occur again. Every real founding claim carries five or six hashes,
// and a vector-of-vectors is exactly where a hand-written BCS layout goes wrong:
// length prefixes, nested sequences, per-element encoding.
//
// These vectors close that gap and two others named as untested:
//
//   - six proof elements, so the non-empty vector<vector<u8>> path is exercised
//   - sequence_number 258, multi-byte, which catches a u64 endianness error that
//     0 cannot
//   - BOTH fee-payer forms
//
// The oracle is the Aptos TS SDK's own generateSigningMessageForTransaction, run
// through the vendored bundle in Aptos-Contracts/tools/wallet-check. A different
// implementation, so agreement is evidence rather than self-consistency.
func TestSigningMessage_MatchesTheProductionShapeVectors(t *testing.T) {
	vs := loadVectors(t)
	seen := map[string]bool{}

	for _, v := range vs {
		seen[v.Name] = true
		amount, err := ParseU64(v.Amount)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FeePayerSigningMessage(TxnFields{
			Sender: v.Sender, SeqNumber: v.Seq, MaxGas: v.MaxGas,
			GasUnitPrice: v.Price, Expiration: v.Expires, ChainID: 2,
			Module: v.Module, ModuleName: "escrow", Function: "claim",
			Escrow: v.Escrow, IdentityHash: v.Ident, AmountMinor: amount,
			Proof: v.Proof, FeePayer: v.FeePayer,
		})
		if err != nil {
			t.Fatalf("%s: encode: %v", v.Name, err)
		}
		want, err := hex.DecodeString(strings.TrimPrefix(v.Message, "0x"))
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Proof) < 2 {
			t.Errorf("%s: proof has %d elements; these vectors exist to exercise the "+
				"NON-EMPTY case", v.Name, len(v.Proof))
		}
		if v.Seq < 256 {
			t.Errorf("%s: sequence_number %d is single-byte; it cannot catch an "+
				"endianness error", v.Name, v.Seq)
		}
		if string(got) != string(want) {
			for i := 0; i < len(got) && i < len(want); i++ {
				if got[i] != want[i] {
					t.Fatalf("%s: first differing byte at %d: got %02x want %02x\n"+
						"  got  %x\n  want %x", v.Name, i, got[i], want[i],
						got[max0(i-6):min(i+10, len(got))], want[max0(i-6):min(i+10, len(want))])
				}
			}
			t.Fatalf("%s: length differs: got %d want %d", v.Name, len(got), len(want))
		}
	}

	// Both forms must be present. A vector file carrying only one would pass
	// while leaving the other untested - the same narrowing a filtered check
	// produces.
	for _, want := range []string{"zero_address", "named_payer"} {
		if !seen[want] {
			t.Errorf("no %q vector; both fee-payer forms must be covered", want)
		}
	}
}

type ed25519PublicKey = ed25519.PublicKey

func deterministicKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func signWith(k ed25519.PrivateKey, msg []byte) []byte { return ed25519.Sign(k, msg) }

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The service must not PICK a fee-payer form. It must establish which one
// arrived, refuse if neither, and record the answer.
func TestVerifySenderForm_IdentifiesWhichFormWasSigned(t *testing.T) {
	// Milestone 1 signed the zero-address form.
	pub := unhex(t, milestone1.PubKey)
	sig := unhex(t, milestone1.Signature)

	f := milestoneFields()
	f.FeePayer = milestone1.FeePayer // the caller supplies the REAL payer

	form, err := VerifySenderForm(f, pub, sig)
	if err != nil {
		t.Fatalf("a genuine signature was refused: %v", err)
	}
	if form != FormZeroAddress {
		t.Fatalf("form = %q, want zero_address — milestone 1 signed 0x0", form)
	}
}

// A signature over neither message must be refused. Submitting something we
// could not verify is buying an abort: the fee payer is charged for a failed
// transaction.
func TestVerifySenderForm_RefusesASignatureOverNeitherForm(t *testing.T) {
	pub := unhex(t, milestone1.PubKey)
	bogus := make([]byte, 64)

	if _, err := VerifySenderForm(milestoneFields(), pub, bogus); err == nil {
		t.Fatal("a signature over neither message was accepted; we would have paid for the abort")
	} else if !strings.Contains(err.Error(), "neither") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// Both forms must be recognised, not just the one we happened to test with.
func TestVerifySenderForm_RecognisesTheNamedPayerForm(t *testing.T) {
	priv := deterministicKey()
	pub := priv.Public().(ed25519PublicKey)

	f := milestoneFields()
	f.FeePayer = milestone1.FeePayer
	msg, err := FeePayerSigningMessage(f) // signed with the REAL payer named
	if err != nil {
		t.Fatal(err)
	}
	form, err := VerifySenderForm(f, pub, signWith(priv, msg))
	if err != nil {
		t.Fatalf("a named-payer signature was refused: %v", err)
	}
	if form != FormNamedPayer {
		t.Fatalf("form = %q, want named_payer", form)
	}
}
