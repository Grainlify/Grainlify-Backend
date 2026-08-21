package sponsor

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aptos-labs/aptos-go-sdk/bcs"
	"golang.org/x/crypto/sha3"
)

// The fee-payer signing message.
//
// # Why the SDK is imported for BCS and nothing else
//
// `github.com/aptos-labs/aptos-go-sdk/bcs` pulls **no third-party modules at
// all** - only the SDK's own `internal/util`. The SDK ROOT package, which
// defines RawTransaction and would hand us this encoding ready-made, pulls
// `coder/websocket` and `hasura/go-graphql-client` for indexer features we do
// not want, in a library that sits next to the sponsor key.
//
// So the struct LAYOUT below is written here and the BCS PRIMITIVES come from
// the SDK. That is a deliberate trade and it has a cost: a wrong layout produces
// a valid signature over the wrong bytes, which submits and then aborts about
// something unrelated. `TestSigningMessage_MatchesTheMilestoneTransaction` is the
// mitigation - a known-good fee-payer transaction from testnet whose recorded
// signature must verify against bytes this function produces.
//
// # The encoding
//
//	sha3_256("APTOS::RawTransactionWithData")
//	  || uleb128(1)                      MultiAgentWithFeePayer
//	  || BCS(RawTransaction)
//	  || BCS(vector<AccountAddress>)     secondary signers, empty
//	  || BCS(AccountAddress)             fee payer
const feePayerDomain = "APTOS::RawTransactionWithData"

// variantMultiAgentWithFeePayer is 1; MultiAgent (no fee payer) is 0. Getting
// this wrong signs a structurally valid message for a different transaction
// kind, and the node rejects it as an invalid signature.
const variantMultiAgentWithFeePayer uint32 = 1

// variantEntryFunction is the TransactionPayload variant index.
const variantEntryFunction uint32 = 2

// ZeroAddress is what a sender signs when they do not know who will pay.
//
// AIP-39 permits signing a fee-payer transaction with fee_payer_address = 0x0,
// the real address being substituted at submission. Verified against the
// milestone transaction: its recorded signature verifies against 0x0 and NOT
// against the sponsor's actual address, which sits in the same authenticator.
//
// Both forms are real. Petra signs the address it is given before signing; the
// TS SDK path signs 0x0. A sponsor service that assumes either one produces a
// valid signature over the wrong bytes, and every claim aborts with
// INVALID_SIGNATURE looking like a wallet fault.
const ZeroAddress = "0x0"

// TxnFields is everything needed to rebuild what was signed.
type TxnFields struct {
	Sender       string
	SeqNumber    uint64
	MaxGas       uint64
	GasUnitPrice uint64
	Expiration   uint64
	ChainID      uint8
	Module       string // address the module is published under
	ModuleName   string // "escrow"
	Function     string // "claim"
	Escrow       string
	IdentityHash string
	AmountMinor  uint64
	Proof        []string
	FeePayer     string
}

func addrBytes(s string) ([]byte, error) {
	h := strings.TrimPrefix(s, "0x")
	// Aptos permits a SHORT address - "0x1", "0x0" - which is odd-length hex and
	// which hex.DecodeString refuses. Pad before decoding, not after: this is the
	// same short-form problem the payout address validator handles, arriving in
	// a different encoder.
	if len(h)%2 == 1 {
		h = "0" + h
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("address %q is not hex: %w", s, err)
	}
	if len(b) > 32 {
		return nil, fmt.Errorf("address %q is longer than 32 bytes", s)
	}
	// Left-pad: Aptos permits a short form, and an address is always 32 bytes on
	// the wire. This is the same canonicalisation the payout address validator
	// applies, for the same reason.
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out, nil
}

// FeePayerSigningMessage produces the exact bytes a sender signs.
func FeePayerSigningMessage(f TxnFields) ([]byte, error) {
	raw, err := rawTransactionBCS(f)
	if err != nil {
		return nil, err
	}
	fp, err := addrBytes(f.FeePayer)
	if err != nil {
		return nil, err
	}

	s := &bcs.Serializer{}
	s.Uleb128(variantMultiAgentWithFeePayer)
	s.FixedBytes(raw)
	s.Uleb128(0) // secondary signers: none
	s.FixedBytes(fp)
	if err := s.Error(); err != nil {
		return nil, err
	}

	h := sha3.New256()
	h.Write([]byte(feePayerDomain))
	return append(h.Sum(nil), s.ToBytes()...), nil
}

func rawTransactionBCS(f TxnFields) ([]byte, error) {
	sender, err := addrBytes(f.Sender)
	if err != nil {
		return nil, err
	}
	payload, err := entryFunctionBCS(f)
	if err != nil {
		return nil, err
	}
	s := &bcs.Serializer{}
	s.FixedBytes(sender)
	s.U64(f.SeqNumber)
	s.FixedBytes(payload)
	s.U64(f.MaxGas)
	s.U64(f.GasUnitPrice)
	s.U64(f.Expiration)
	s.U8(f.ChainID)
	return s.ToBytes(), s.Error()
}

func entryFunctionBCS(f TxnFields) ([]byte, error) {
	modAddr, err := addrBytes(f.Module)
	if err != nil {
		return nil, err
	}
	args, err := claimArgsBCS(f)
	if err != nil {
		return nil, err
	}
	s := &bcs.Serializer{}
	s.Uleb128(variantEntryFunction)
	s.FixedBytes(modAddr)
	s.WriteString(f.ModuleName)
	s.WriteString(f.Function)
	s.Uleb128(0) // type arguments: none
	s.Uleb128(uint32(len(args)))
	for _, a := range args {
		// Each argument is itself length-prefixed: entry function args are
		// vector<vector<u8>>, so an already-BCS-encoded value is wrapped again.
		s.WriteBytes(a)
	}
	return s.ToBytes(), s.Error()
}

// claimArgsBCS encodes claim(escrow, identity_hash, amount, proof).
func claimArgsBCS(f TxnFields) ([][]byte, error) {
	escrow, err := addrBytes(f.Escrow)
	if err != nil {
		return nil, err
	}
	ident, err := hex.DecodeString(strings.TrimPrefix(f.IdentityHash, "0x"))
	if err != nil {
		return nil, fmt.Errorf("identity hash is not hex: %w", err)
	}

	// address: 32 raw bytes, no inner length prefix.
	a0 := escrow

	// vector<u8>: uleb128 length then bytes.
	s1 := &bcs.Serializer{}
	s1.WriteBytes(ident)
	a1 := s1.ToBytes()

	// u64, little-endian.
	s2 := &bcs.Serializer{}
	s2.U64(f.AmountMinor)
	a2 := s2.ToBytes()

	// vector<vector<u8>>.
	s3 := &bcs.Serializer{}
	s3.Uleb128(uint32(len(f.Proof)))
	for _, p := range f.Proof {
		b, err := hex.DecodeString(strings.TrimPrefix(p, "0x"))
		if err != nil {
			return nil, fmt.Errorf("proof element %q is not hex: %w", p, err)
		}
		s3.WriteBytes(b)
	}
	a3 := s3.ToBytes()

	for _, s := range []*bcs.Serializer{s1, s2, s3} {
		if err := s.Error(); err != nil {
			return nil, err
		}
	}
	return [][]byte{a0, a1, a2, a3}, nil
}

// ParseU64 is a small helper: amounts arrive as decimal strings because a u64
// does not fit a float64.
func ParseU64(s string) (uint64, error) { return strconv.ParseUint(s, 10, 64) }

// FeePayerForm records which of the two legal shapes a sender actually signed.
//
// Not a preference and not a configuration value: a fact about the submission in
// hand, established by checking rather than assumed.
type FeePayerForm string

const (
	// FormZeroAddress: the sender signed fee_payer_address = 0x0, not knowing
	// who would pay. The TS SDK path does this.
	FormZeroAddress FeePayerForm = "zero_address"
	// FormNamedPayer: the sender signed the real fee payer address, having been
	// given it first. Petra does this.
	FormNamedPayer FeePayerForm = "named_payer"
)

// ErrSignatureMatchesNeitherForm means the sender's signature is not over either
// legal fee-payer message.
//
// **Refusing here is the only thing between a malformed submission and a
// transaction we pay for that then aborts.** A fee payer is charged for a failed
// transaction, so submitting something we could not verify is us buying an
// abort.
var ErrSignatureMatchesNeitherForm = errors.New("sender_signature_invalid")

// VerifySenderForm establishes WHICH fee-payer message the sender signed.
//
// # Why this checks both instead of choosing one
//
// Both forms are live in this workstream: Petra had to be given the fee payer
// address before signing and therefore signs the real one, while the TS SDK path
// used for the first milestone signs 0x0. Assuming either makes the service work
// against whichever half it was tested with and fail against the other - and the
// failure is INVALID_SIGNATURE at submission, which reads to a contributor as a
// broken wallet rather than as our bug.
//
// Checking both costs two ed25519 verifications, which is nothing next to the
// transaction we are about to pay for.
//
// The form is returned so the caller can RECORD it. That record is what tells us
// later whether a wallet changed behaviour, which is not a question anybody can
// answer retrospectively from an INVALID_SIGNATURE.
func VerifySenderForm(f TxnFields, senderPubKey, senderSig []byte) (FeePayerForm, error) {
	named := f
	zero := f
	zero.FeePayer = ZeroAddress

	for form, fields := range map[FeePayerForm]TxnFields{
		FormZeroAddress: zero,
		FormNamedPayer:  named,
	} {
		msg, err := FeePayerSigningMessage(fields)
		if err != nil {
			return "", err
		}
		if ed25519.Verify(senderPubKey, msg, senderSig) {
			return form, nil
		}
	}
	return "", fmt.Errorf("%w: the signature is over neither the zero-address nor the "+
		"named-payer message, so nothing is submitted", ErrSignatureMatchesNeitherForm)
}
