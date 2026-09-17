package handlers

import (
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/sha3"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

// The contract between the browser and verifyEVM.
//
// The frontend (src/shared/wallet/evm.ts) sends the challenge's `message` field
// to the wallet through personal_sign as 0x + hex of its UTF-8 bytes, and posts
// back whatever signature the wallet returns. Nothing else in either codebase
// asserts that the digest a wallet signs is the digest verifyEVM recovers
// against, and a mismatch fails every registration as `signature_invalid` with
// nothing pointing at the message format.
//
// # Why the EIP-191 prefix is written out by hand
//
// walletPersonalSign spells the prefix literally instead of calling
// accounts.TextHash. verifyEVM hashes with TextHash, so a test that signed with
// TextHash would pass whatever TextHash did - including if it, or verifyEVM's
// choice of it, stopped matching what wallets compute. The literal prefix is
// the wallet's side of the contract, written down independently. Do not
// "simplify" it into TextHash; registerEVM in payout_address_evm_test.go
// already does that and pins nothing here.

// walletPersonalSign does what a browser wallet does for
// personal_sign(hexMessage, address): hex-decode the payload to bytes, sign
// keccak256("\x19Ethereum Signed Message:\n" + decimal byte length + bytes),
// and return r||s||v with v in {27,28}, 0x-prefixed.
func walletPersonalSign(t *testing.T, hexMessage string, key []byte) string {
	t.Helper()
	payload, err := hex.DecodeString(strings.TrimPrefix(hexMessage, "0x"))
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(payload))))
	h.Write(payload)

	priv, err := crypto.ToECDSA(key)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	sig, err := crypto.Sign(h.Sum(nil), priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig)
}

// browserHex is what evm.ts sends: 0x + hex of the UTF-8 bytes of the message.
func browserHex(message string) string {
	return "0x" + hex.EncodeToString([]byte(message))
}

func encodingFixture(t *testing.T) (key []byte, postedAddr, message string) {
	t.Helper()
	key = crypto.Keccak256([]byte("payout-address-evm-encoding"))
	priv, err := crypto.ToECDSA(key)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	// Wallets hand out lowercase addresses; the server canonicalises to EIP-55
	// before building the challenge, and again before rebuilding it.
	postedAddr = strings.ToLower(crypto.PubkeyToAddress(priv.PublicKey).Hex())
	canonical, err := payoutaddr.ValidateEVM(postedAddr)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return key, postedAddr, challengeMessage("evm-encoding", canonical, "n0nce_-AZaz09")
}

// verifyAsServer mirrors PostAddress: canonicalise the posted address, rebuild
// the message, verify through the same call verifyForFamily makes.
func verifyAsServer(t *testing.T, postedAddr, signature string) error {
	t.Helper()
	canonical, err := payoutaddr.ValidateEVM(postedAddr)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	msg := challengeMessage("evm-encoding", canonical, "n0nce_-AZaz09")
	return auth.VerifySignature(auth.WalletTypeEVM, canonical, msg, signature, "")
}

func TestEVMEncoding_HexUTF8PersonalSignVerifies(t *testing.T) {
	key, posted, msg := encodingFixture(t)
	sig := walletPersonalSign(t, browserHex(msg), key)

	if err := verifyAsServer(t, posted, sig); err != nil {
		t.Fatalf("a wallet signature over the issued message must verify: %v", err)
	}
	// Posted address case must not matter: the server normalises it.
	canonical, _ := payoutaddr.ValidateEVM(posted)
	if err := verifyAsServer(t, canonical, sig); err != nil {
		t.Fatalf("checksummed posted address must verify too: %v", err)
	}
}

// Each case is a plausible edit - to the client, or to challengeMessage without
// updating the client - that would fail every signature.
func TestEVMEncoding_MessageMustBeSignedByteForByte(t *testing.T) {
	key, posted, msg := encodingFixture(t)
	canonical, _ := payoutaddr.ValidateEVM(posted)

	cases := map[string]string{
		"CRLF line endings": strings.ReplaceAll(msg, "\n", "\r\n"),
		"trailing newline":  msg + "\n",
		"lowercased address inside the message": strings.Replace(
			msg, canonical, strings.ToLower(canonical), 1),
	}
	for name, signed := range cases {
		t.Run(name, func(t *testing.T) {
			if signed == msg {
				t.Fatal("case does not change the message; it pins nothing")
			}
			sig := walletPersonalSign(t, browserHex(signed), key)
			if err := verifyAsServer(t, posted, sig); err == nil {
				t.Fatal("a signature over an altered message verified")
			}
		})
	}
}

// The signature goes back exactly as the wallet returned it. verifyEVM decodes
// with hexutil.Decode, which requires the 0x prefix.
func TestEVMEncoding_SignatureKeepsItsPrefix(t *testing.T) {
	key, posted, msg := encodingFixture(t)
	sig := walletPersonalSign(t, browserHex(msg), key)

	if err := verifyAsServer(t, posted, strings.TrimPrefix(sig, "0x")); err == nil {
		t.Fatal("an unprefixed signature verified; evm.ts's rule to send it unmodified is now stale")
	}
}
