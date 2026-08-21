// Package payout turns a settled event into a Merkle tree, its persisted leaves
// and a published-root record.
//
// # It does not know what a founding pool is
//
// This consumes a producer-neutral shape: one row per payable contributor per
// event, carrying a settlement id, a user id and an integer minor-unit amount,
// plus an event-level chain id. `internal/founding` is one producer of that
// shape; a hackathon settlement will be another, and neither is privileged here.
//
// That is deliberate and it is the expensive-to-reverse decision in this
// package. Built against founding.Result directly, the second producer would
// arrive with its own tree path, and the two would disagree the first time
// anybody compared them - which is the drift this repository has already paid
// for once, between the Go leaf builder and the Soroban contract.
//
// So nothing founding-specific may appear below this line. In particular:
//
//   - **pool_usdc and share_value_usdc** are decimal USDC and belong to how
//     founding computes a settlement. Money here is integer minor units only.
//   - **Band multipliers and wave assignment** are how founding decides who is
//     owed what. This package receives the answer, never the derivation.
//   - **founding_members / founding_shares** are a producer's own tables. The
//     only tables this package reads for people are github_accounts and
//     contributor_addresses, which are producer-independent.
//
// If any of those needs to be visible here, the shape is wrong and the fix is to
// widen Settlement, not to import founding.
package payout

import (
	"math/big"

	"github.com/google/uuid"
)

// Outcome is what happened to one member. Every member gets exactly one.
//
// This is a classification, not a filter. A filter answers "who is in the tree";
// these answer "what happened to each person", and the difference matters
// because two of these outcomes describe somebody who earned a real amount and
// will not receive it. Dropping them from a list would make the totals correct
// and the record wrong.
type Outcome string

const (
	// OutcomePayable is in the tree.
	OutcomePayable Outcome = "payable"
	// OutcomeNoShares earned nothing: the allocation rounded to zero.
	OutcomeNoShares Outcome = "no_shares"
	// OutcomeIneligible earned nothing: excluded by the producer's own rule.
	OutcomeIneligible Outcome = "ineligible"

	// OutcomeNoAddress earned an amount and has nowhere to receive it.
	//
	// NOT a variant of ineligible. Those people earned nothing; this person
	// earned something we cannot deliver.
	OutcomeNoAddress Outcome = "no_address"

	// OutcomeNoGitHubAccount earned an amount and has no GitHub login, so no
	// identity hash can be computed for them.
	//
	// Reachable by design rather than only by corruption: account deletion
	// leaves an anonymised tombstone - the users row is retained with
	// identifying columns nulled and github_accounts is hard deleted along with
	// its token - so a member who closes their account keeps their shares and
	// loses their login. That is why this is a classification and not an error:
	// aborting the whole build over one row is the wrong failure mode when the
	// entitlement is real.
	OutcomeNoGitHubAccount Outcome = "no_github_account"

	// OutcomeKYCUnresolved earned an amount and is not currently verified.
	//
	// # Why this is here and not in founding.Eligible
	//
	// This is the placement, and it is not a taxonomy preference. Adding a KYC
	// check to Eligible zeroes the line's effective weight; a zero weight is
	// out of the divisor; Apportion then divides the whole pool across the
	// remaining lines and asserts it allocated all of it. **That hands this
	// person's money to the other contributors, permanently** - recovering it
	// would mean taking it back from people who did nothing wrong.
	//
	// Resolved here, the person keeps a positive weight and a real allocation,
	// gets no leaf, and their amount lands in residue. The escrow is funded
	// with LeafTotalMinor, so the money never leaves the treasury and is never
	// given to anybody else. That is what makes a hold possible at all.
	//
	// Anyone moving this check into Eligible has converted a hold into a
	// forfeit, and the diff will not look like it.
	//
	// # Why held rather than excluded
	//
	// We reset people for benign reasons - a cropped scan, an unreadable photo
	// - so a settlement running mid-reset would sweep somebody for a
	// photograph. Held, a benign reset resolves and pays late while a genuine
	// refusal never resolves and never pays: both correct outcomes fall out of
	// one rule, and the rule never has to tell them apart.
	OutcomeKYCUnresolved Outcome = "kyc_unresolved"
)

// Owed reports whether this outcome describes somebody who earned an amount they
// will not receive. Those are the rows a human must read before publication.
func (o Outcome) Owed() bool {
	return o == OutcomeNoAddress || o == OutcomeNoGitHubAccount || o == OutcomeKYCUnresolved
}

// Entitlement is one payable person in one event, as a producer computes them.
//
// # This type lives here, and producers import it
//
// It is deliberately defined in the consumer, not in each producer. A producer
// that defines its own entitlement type and converts at the boundary reintroduces
// exactly what this package exists to prevent: two shapes that agree today,
// drift quietly, and disagree the first time anybody compares two events. If a
// producer needs a field this does not have, add it here.
//
// # Where the amount must come from
//
// AmountMinor is the output of the shared apportion rule and nothing else.
//
// In particular it is NOT hackathon_verdicts.payout_amount: ComputePayout has
// already rounded that once, and re-apportioning a rounded figure rounds a
// rounding. The path is effective_units -> apportion -> AmountMinor, and
// **apportion is the only place a fraction becomes an integer.** A second
// rounding does not announce itself - the totals still look plausible, they are
// just wrong by a few minor units per person, which is the family of fault that
// presents as a legitimate value.
type Entitlement struct {
	UserID uuid.UUID
	// AmountMinor is exact integer minor units. Never a float, never decimal
	// USDC: those belong to whoever computed this.
	AmountMinor *big.Int
	// IneligibleReason is the producer's own explanation for a zero amount, if
	// it has one. Carried through so the report can repeat it, never
	// interpreted here.
	IneligibleReason string
}

// Settlement is one event's worth of entitlements from any producer.
type Settlement struct {
	SettlementID uuid.UUID
	ChainID      string
	// PoolMinor is the whole budget, used only to reconcile the report. The sum
	// of entitlements may be less than this; the difference is residue.
	PoolMinor     *big.Int
	AssetDecimals int32
	Pool          string // "contributor" or "maintainer"
	Entitlements  []Entitlement
}

// Member is one entitlement after the identity and address joins, with its
// outcome decided.
type Member struct {
	UserID       uuid.UUID
	GitHubLogin  string // empty when there is no github_accounts row
	ClaimAddress string // canonical form, empty when none is registered
	AmountMinor  *big.Int
	Outcome      Outcome
	// Reason is the producer's ineligible reason, for OutcomeIneligible only.
	Reason string
}
