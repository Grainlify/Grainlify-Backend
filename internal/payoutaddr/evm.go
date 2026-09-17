package payoutaddr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// EVM payout destinations, kept in a function of their own rather than taught
// to Validate.
//
// # Why this is not a branch inside Validate
//
// Validate canonicalises for APTOS, and its central move is to left-pad a short
// address to 64 hex characters - correct there, because 0x1 and its padded form
// are genuinely the same Aptos address.
//
// "0x" followed by 40 hex characters is also exactly the shape of an EVM
// address, and Validate cannot tell which one the person meant. Handed an EVM
// address it returns a success:
//
//	0x106175F175B940CcA1816d75eB19937a88BE7720
//	-> 0x000000000000000000000000106175f175b940cca1816d75eb19937a88be7720
//
// That is a legal Aptos address with nothing to do with whoever typed the
// input, and nothing flags it. See Grainlify-Backend#548.
//
// So the split is not stylistic. One function that pads and one that must never
// pad cannot be the same function, and the way this goes wrong is silent: the
// value is well-formed, it stores, and it is simply somebody else's address.
//
// **No EVM path may call Validate or Canonical.** TestNoEVMPathPads pins it.
var (
	// ErrEVMLength is a length that is not exactly 40 hex characters.
	//
	// Separate from ErrTooLong because the remedy differs. On Aptos "too long"
	// means the input is not an address at all; here a 39- or 41-character
	// value is nearly always a truncated or doubled paste, and the person needs
	// to be told the exact length required rather than that theirs was wrong.
	ErrEVMLength = errors.New("an EVM payout address must be 0x followed by exactly 40 hex characters")

	// ErrEVMChecksum is mixed-case input whose EIP-55 checksum does not verify.
	//
	// This is the one check that catches a MISTYPED address. Every other rule
	// here passes any 40 hex characters, so without this a single wrong
	// character produces a perfectly valid address belonging to nobody, and the
	// money goes there permanently. EIP-55 is the only integrity information an
	// EVM address carries.
	ErrEVMChecksum = errors.New("EVM payout address failed its EIP-55 checksum")

	// ErrEVMZero is the zero address.
	//
	// The EVM counterpart of ErrReserved: 0x0 is the burn address, tokens sent
	// there are unrecoverable by the person owed them, and it is also what an
	// uninitialised variable serialises to - so it is the single most likely
	// wrong value to arrive here, and the one with no remedy.
	ErrEVMZero = errors.New("EVM payout address is the zero address")
)

// ValidateEVM checks an EVM address and returns its EIP-55 checksummed form.
//
// # What it returns, and why not lowercase
//
// The stored form is the mixed-case EIP-55 spelling, CONSTRUCTED here by
// go-ethereum rather than echoed from the input. That keeps the repository's
// standing rule - a value crossing a typed boundary is constructed, never
// spelled - and it means the stored value carries its own checksum, so a
// corrupted row is detectable later rather than merely being 40 plausible
// characters.
//
// Case therefore matters to the bytes but must NOT matter to uniqueness: two
// spellings of one address are one address, so every uniqueness rule over this
// column compares lower(address). The migration that relaxes the all-lowercase
// CHECK moves those indexes in the same change, which is what migration 000086
// asked for in writing.
//
// # What is deliberately NOT done
//
// No padding, in any direction, ever. Not of a short input up to 40, and not of
// 40 up to 64. A value that is not already exactly 40 hex characters is an
// error, never something to be repaired - repairing it is precisely the defect
// in #548.
func ValidateEVM(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmpty
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return "", fmt.Errorf("%w: %q", ErrNoPrefix, raw)
	}
	body := s[2:]
	for _, c := range body {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", fmt.Errorf("%w: %q", ErrNotHex, raw)
		}
	}
	// Checked BEFORE anything else interprets the value. common.HexToAddress
	// silently left-pads and truncates, so calling it on an unchecked string is
	// the same class of bug this function exists to prevent - it would turn a
	// 39-character typo into a valid address rather than an error.
	if len(body) != 40 {
		return "", fmt.Errorf("%w: got %d", ErrEVMLength, len(body))
	}

	lower := strings.ToLower(body)
	upper := strings.ToUpper(body)
	// Mixed case is a CLAIM that the input carries an EIP-55 checksum, so it is
	// verified. All-lowercase and all-uppercase carry no checksum information at
	// all and cannot be verified against anything - refusing them would refuse
	// the form most wallets and block explorers still hand out.
	if body != lower && body != upper {
		if common.HexToAddress(s).Hex() != s {
			return "", fmt.Errorf("%w: %q. Copy it again from your wallet - one wrong "+
				"character produces a valid address belonging to somebody else, and a "+
				"payout sent there cannot be recovered", ErrEVMChecksum, raw)
		}
	}

	addr := common.HexToAddress(s)
	if addr == (common.Address{}) {
		return "", fmt.Errorf("%w: a payout sent there is unrecoverable", ErrEVMZero)
	}
	return addr.Hex(), nil
}
