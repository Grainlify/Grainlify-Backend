package payout

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// InputDigest is a commitment to what the dry run READ, so that building from a
// report somebody approved cannot silently build from something else.
//
// # It digests inputs, not the tree
//
// Every member is included regardless of outcome, and what is hashed is the
// joined facts - user, login, address, amount - not the classification derived
// from them.
//
// Digesting the emitted leaves instead would miss the mismatch most worth
// catching. Somebody who moves between classifications changes both the leaf set
// and the excluded set, and a digest over one side alone can come out the same
// while the report is now wrong about two people. Hashing the inputs means any
// change that could move anybody shows up, whichever direction it moves them.
//
// # A login rename lands here, and that is correct
//
// identity_hash is H(lower(login) || salt), so a GitHub rename between report and
// build changes an input and this digest with it. The build then refuses and the
// report must be read again.
//
// That is the right outcome rather than a spurious failure: the rename changes
// which identity hash goes into a permanent root. Re-running the dry run is
// cheap; a root committing to the wrong identity cannot be edited.
func InputDigest(s Settlement, members []Member) string {
	h := sha256.New()
	// The header binds the digest to this settlement and chain, so a report for
	// one event cannot approve a build for another.
	fmt.Fprintf(h, "grainlify.payout.inputs.v1\n%s\n%s\n%s\n", s.SettlementID, s.ChainID, s.Pool)
	for _, m := range members {
		// Boundaries are recoverable two ways at once: a length prefix on each
		// variable-length field, and a newline delimiter that no field can
		// contain (GitHub logins are alphanumeric with hyphens, addresses are
		// regex-checked hex). Concatenating without either makes the boundaries
		// unrecoverable - the defect the leaf construction was fixed for.
		//
		// Note for anyone mutation-testing this: removing EITHER mechanism alone
		// is undetectable, because the other still separates the fields. Only
		// removing both collides, which is what the boundary test mutates away.
		// That is the fail-closed-default trap in docs/VERIFICATION-TRAPS.md -
		// with belt and braces on you cannot tell from outside which is holding.
		// The prefixes stay because the newline guarantee depends on a charset
		// assumption that a future free-text field would silently break. The
		// mutation harness therefore tests the PROPERTY - remove both and the
		// digest collides - rather than either mechanism, because a mutation
		// aimed at one alone cannot honestly be killed.
		fmt.Fprintf(h, "%s\n%d:%s\n%d:%s\n%s\n",
			m.UserID,
			len(m.GitHubLogin), m.GitHubLogin,
			len(m.ClaimAddress), m.ClaimAddress,
			m.AmountMinor.String())
	}
	return hex.EncodeToString(h.Sum(nil))
}
