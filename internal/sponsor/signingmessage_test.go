package sponsor

import (
	"crypto/ed25519"
	"encoding/hex"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// milestone1 is a REAL fee-payer transaction on Aptos testnet, recorded in
// Aptos-Contracts/README.md, produced by scripts/sponsored-claim.js.
//
//	https://explorer.aptoslabs.com/txn/0x1be03bd4…3396ba?network=testnet
//
// The claimant had never transacted: sequence_number 0, no APT, no account on
// chain until this transaction created one. They received 1,000,000 USDC units
// and paid nothing.
var milestone1 = struct {
	Sender, PubKey, Signature   string
	Seq, MaxGas, Price, Expires uint64
	Module, Escrow, Ident       string
	Amount                      uint64
	FeePayer                    string
	ChainID                     uint8
}{
	Sender: "0xd46acd056131049197eeeb3a784940a3104ebed923dacfbc013898756b9004dd",
	PubKey: "0x4eb36b2718e92cb9338bf45177e50a9a23a265416a20d1e92a90f8592b8c6432",
	Signature: "0x5a1e969f88b2fa20fdaf0f5b5131a53126abae8733d723531e0f3c141038f117" +
		"1adfc1e50bdfd0c30fe770638aa552105da1d251b9e31c1b30223a963467b70e",
	Seq: 0, MaxGas: 20000, Price: 100, Expires: 1787028364,
	Module:   "0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9",
	Escrow:   "0xdd4fa63ec44e4726cd7d1118596866cd0c5d4cd4d3fcc9762a42261872795f45",
	Ident:    "0x2222222222222222222222222222222222222222222222222222222222222222",
	Amount:   1000000,
	FeePayer: "0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9",
	ChainID:  2, // testnet
}

func milestoneFields() TxnFields {
	return TxnFields{
		Sender: milestone1.Sender, SeqNumber: milestone1.Seq,
		MaxGas: milestone1.MaxGas, GasUnitPrice: milestone1.Price,
		Expiration: milestone1.Expires, ChainID: milestone1.ChainID,
		Module: milestone1.Module, ModuleName: "escrow", Function: "claim",
		Escrow: milestone1.Escrow, IdentityHash: milestone1.Ident,
		AmountMinor: milestone1.Amount, Proof: []string{},
		FeePayer: ZeroAddress,
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// THE GOLDEN VECTOR.
//
// # What this proves
//
// That our BCS encoding of the fee-payer signing message is BYTE-IDENTICAL to
// the bytes a real wallet signed, for these inputs. Ed25519 verification is over
// exact bytes: one byte different anywhere - a wrong enum variant, a missing
// length prefix, a big-endian u64, the wrong domain separator - and this fails.
//
// It is an external oracle. The signature was produced by other software
// (sponsored-claim.js and the Aptos TS SDK), recorded on a public chain, and
// cannot be adjusted to make our encoder look right.
//
// # What this does NOT prove
//
// **It proves the encoding for THESE inputs and no others.** The milestone
// exercised exactly one shape:
//
//   - one entry-function call, four arguments
//   - an EMPTY proof array - so the vector<vector<u8>> path is only tested at
//     length zero, and a length-prefix bug in the non-empty case would survive
//   - a 32-byte identity hash and a u64 amount
//   - no type arguments
//   - sequence_number 0, which is a single byte and would not catch a u64
//     endianness error on its own
//   - the ZERO fee-payer form only. The other form, where the sender signs the
//     real fee payer address, is unexercised by this vector and needs its own.
//
// A different payload shape, a multi-argument call with other types, or a proof
// of length three are unexercised. **Do not read one green vector as coverage of
// the encoder.** The honest scope is: this specific transaction re-encodes
// correctly, which is enough to rule out a systematically wrong layout and not
// enough to rule out a shape-specific one.
// # THE SENDER SIGNS WITH FEE PAYER 0x0
//
// Found by this vector failing, which is what it is for.
//
// AIP-39 allows a sender to sign a fee-payer transaction WITHOUT knowing who
// will pay: they sign with fee_payer_address = 0x0, and the real address is
// substituted at submission. The milestone transaction was produced that way -
// the recorded signature verifies against 0x0 and NOT against the sponsor's
// actual address, which is stored in the very same authenticator.
//
// **Both forms exist in the wild.** The Petra wallet check earlier in this
// workstream had to set feePayerAddress BEFORE signing, so Petra signs the real
// address; the TS SDK path used by scripts/sponsored-claim.js signs 0x0. A
// production sponsor service must accept both, and assuming either one is the
// "valid signature over the wrong bytes" failure again - every claim would abort
// with INVALID_SIGNATURE and read as a wallet bug.
func TestSigningMessage_MatchesTheMilestoneTransaction(t *testing.T) {
	msg, err := FeePayerSigningMessage(milestoneFields())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pub := ed25519.PublicKey(unhex(t, milestone1.PubKey))
	sig := unhex(t, milestone1.Signature)

	if !ed25519.Verify(pub, msg, sig) {
		t.Fatalf("the recorded signature does not verify against our encoding.\n"+
			"  Our message is %d bytes, sha3 prefix %x…\n"+
			"  A signature that verifies is the ONLY evidence the bytes match; a\n"+
			"  self-consistent wrong encoding would sign, submit, and abort about\n"+
			"  something unrelated.", len(msg), msg[:8])
	}
}

// A wrong domain separator produces a valid signature over the wrong bytes -
// the exact failure this vector exists to catch. Proves the vector is sensitive.
func TestSigningMessage_TheVectorIsSensitiveToTheEncoding(t *testing.T) {
	base, err := FeePayerSigningMessage(milestoneFields())
	if err != nil {
		t.Fatal(err)
	}
	pub := ed25519.PublicKey(unhex(t, milestone1.PubKey))
	sig := unhex(t, milestone1.Signature)

	for name, mutate := range map[string]func(TxnFields) TxnFields{
		"sequence number": func(f TxnFields) TxnFields { f.SeqNumber = 1; return f },
		"amount":          func(f TxnFields) TxnFields { f.AmountMinor = 999999; return f },
		"chain id":        func(f TxnFields) TxnFields { f.ChainID = 1; return f },
		"fee payer":       func(f TxnFields) TxnFields { f.FeePayer = milestone1.FeePayer; return f },
		"escrow":          func(f TxnFields) TxnFields { f.Escrow = milestone1.Module; return f },
		"gas price":       func(f TxnFields) TxnFields { f.GasUnitPrice = 101; return f },
		"a proof element": func(f TxnFields) TxnFields { f.Proof = []string{milestone1.Ident}; return f },
	} {
		m, err := FeePayerSigningMessage(mutate(milestoneFields()))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ed25519.Verify(pub, m, sig) {
			t.Errorf("changing the %s still verified; the vector is not sensitive to it", name)
		}
		if string(m) == string(base) {
			t.Errorf("changing the %s did not change the message", name)
		}
	}
}

// # Why the SDK root is not imported
//
// `aptos-go-sdk/bcs` pulls no third-party modules. The SDK ROOT pulls
// `coder/websocket` and `hasura/go-graphql-client` for indexer features we do not
// use - in a library that sits beside the sponsor key.
//
// "We don't call the websocket client" is a convention. "No import path reaches
// it" is a property, and it is the same distinction as two call sites agreeing
// versus a shape that cannot disagree. This test keeps it a property.
//
// If you are adding an SDK import and this test fails: the transaction types you
// want live in the root package, and taking them costs a websocket and a GraphQL
// client in the build. That trade was made deliberately once - see the layout
// written by hand in signingmessage.go, and the golden vector above that makes it
// safe. Make it deliberately again or not at all.
func TestSDKSurface_OnlyBCSIsImported(t *testing.T) {
	var offenders []string
	root := ".."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, "github.com/aptos-labs/aptos-go-sdk") {
				continue
			}
			if p != "github.com/aptos-labs/aptos-go-sdk/bcs" {
				offenders = append(offenders, path+" imports "+p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("only aptos-go-sdk/bcs may be imported: %s", o)
	}
}
