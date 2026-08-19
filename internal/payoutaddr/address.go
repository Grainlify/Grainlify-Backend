// Package payoutaddr validates and canonicalises payout destinations.
//
// Kept separate from sign-in wallets on purpose: `wallets` answers "prove you
// hold this key", and a payout address answers "send money here". See
// migrations/000080 and docs/SCOPE-salt-and-addresses.md.
package payoutaddr

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	ErrEmpty    = errors.New("payout address is empty")
	ErrNotHex   = errors.New("payout address is not hexadecimal")
	ErrTooLong  = errors.New("payout address is longer than 32 bytes")
	ErrReserved = errors.New("payout address is a reserved framework address")
	ErrNoPrefix = errors.New("payout address must start with 0x")
)

// ReservedBelow is the exclusive upper bound of the Aptos reserved range.
//
// 0x0 through 0xf are the framework and special addresses - 0x1 is the Aptos
// framework itself, 0x3 and 0x4 are token standards, 0x0 is the burn hole.
// Tokens sent to any of them are unrecoverable by the person who was owed them,
// and nothing in the escrow can undo a claim once it lands.
const ReservedBelow = 0x10

// Canonical returns the 0x-prefixed, 64-lowercase-hex form of an Aptos address.
//
// Aptos permits an address to be written short - 0x1 and the 64-character padded
// form are the same address - so a payout destination must be CONSTRUCTED into
// one representation, never stored as the user spelled it. Two spellings of one
// address defeat the unique index and produce a leaf committing to a string the
// wallet will not match at claim time.
//
// This is the same rule the wallet check arrived at after four instances of the
// opposite: a value crossing a typed boundary must be constructed, never
// spelled.
func Canonical(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmpty
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return "", fmt.Errorf("%w: %q", ErrNoPrefix, raw)
	}
	body := strings.ToLower(s[2:])
	if body == "" {
		return "", fmt.Errorf("%w: %q", ErrEmpty, raw)
	}
	for _, c := range body {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("%w: %q", ErrNotHex, raw)
		}
	}
	if len(body) > 64 {
		return "", fmt.Errorf("%w: %d hex characters", ErrTooLong, len(body))
	}
	return "0x" + strings.Repeat("0", 64-len(body)) + body, nil
}

// Validate canonicalises and then refuses the reserved range.
//
// Enforced here, where the person can still fix it, and asserted again at tree
// build as defence in depth - a leaf commits permanently and a root cannot be
// edited.
func Validate(raw string) (string, error) {
	c, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	n, ok := new(big.Int).SetString(c[2:], 16)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotHex, raw)
	}
	if n.Cmp(big.NewInt(ReservedBelow)) < 0 {
		return "", fmt.Errorf("%w: %s is below 0x%x and belongs to the framework; "+
			"a payout sent there cannot be recovered", ErrReserved, c, ReservedBelow)
	}
	return c, nil
}
