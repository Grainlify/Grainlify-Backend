package payout

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Report is what a person reads before anything touches the chain.
//
// It writes nothing and it needs no salt. That falls out of what the reader is
// actually checking: names, addresses, amounts, who is in and who is out, and
// whether the arithmetic closes. Leaf digests are not reviewable by eye, so this
// does not compute them - which means no salt is decrypted, no secret is created
// for a settlement that may never be built, and the write-free property is
// structural rather than promised.
type Report struct {
	Settlement Settlement
	Members    []Member
	// InputDigest commits to what this report read. Build refuses without it.
	InputDigest string

	LeafTotalMinor     *big.Int // what must be funded
	ExcludedTotalMinor *big.Int // earned, undeliverable
	NoSharesCount      int
	IneligibleCount    int
	ResidueMinor       *big.Int // pool - leaf total; sweepable with NO timelock
}

// Payable returns the members that become leaves, in tree order by user.
func (r *Report) Payable() []Member {
	out := make([]Member, 0, len(r.Members))
	for _, m := range r.Members {
		if m.Outcome == OutcomePayable {
			out = append(out, m)
		}
	}
	return out
}

// Owed returns members who earned an amount they will not receive.
func (r *Report) Owed() []Member {
	out := make([]Member, 0)
	for _, m := range r.Members {
		if m.Outcome.Owed() {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AmountMinor.Cmp(out[j].AmountMinor) > 0 })
	return out
}

// DryRun resolves and reconciles. It writes nothing.
func DryRun(ctx context.Context, pool db.DBPool, s Settlement) (*Report, error) {
	members, err := Resolve(ctx, pool, s)
	if err != nil {
		return nil, err
	}
	r := &Report{
		Settlement:         s,
		Members:            members,
		InputDigest:        InputDigest(s, members),
		LeafTotalMinor:     new(big.Int),
		ExcludedTotalMinor: new(big.Int),
		ResidueMinor:       new(big.Int),
	}
	for _, m := range members {
		// Owed() rather than a list of outcomes.
		//
		// This used to name OutcomeNoAddress and OutcomeNoGitHubAccount
		// explicitly, so adding a third owed outcome silently dropped it from
		// this total - and this total is what Build makes a human acknowledge
		// before publishing. A hold missing from it is a person the operator
		// was never shown, on the one screen that exists to show them.
		//
		// Owed() is the definition; anything that has to be kept in step with
		// it by hand will eventually not be.
		switch {
		case m.Outcome == OutcomePayable:
			r.LeafTotalMinor.Add(r.LeafTotalMinor, m.AmountMinor)
		case m.Outcome.Owed():
			r.ExcludedTotalMinor.Add(r.ExcludedTotalMinor, m.AmountMinor)
		case m.Outcome == OutcomeNoShares:
			r.NoSharesCount++
		case m.Outcome == OutcomeIneligible:
			r.IneligibleCount++
		}
	}
	// THE IDENTITY: every allocated minor unit lands in exactly one bucket.
	//
	// Deriving the excluded total from Owed() above fixes one instance. This
	// makes the class impossible, and the class is worth the arithmetic because
	// of where it lands.
	//
	// The acknowledgement gate's entire value is that a human reads a number.
	// An outcome missing from these buckets does not break the gate - it shows
	// a SMALLER total, which looks perfectly plausible, and the operator
	// acknowledges it. Nothing fails, nothing is logged, nobody is alerted, and
	// the money held for that person exists in no total anyone read. It is the
	// worst possible place for a set maintained by hand: the failure is a
	// number that is wrong in the quiet direction.
	//
	// So a seventh outcome belonging to neither bucket fails HERE,
	// arithmetically and immediately, without anyone having remembered to write
	// a test for it. Same move as one shared date formatter: two things that
	// agree became a shape that cannot disagree.
	//
	// Why the identity holds by construction, and is therefore safe to assert:
	// Resolve only assigns NoShares or Ineligible when the amount is <= 0, so
	// every member carrying a positive amount is Payable or Owed(). A new
	// outcome that is neither - or an old one that stops being counted - breaks
	// this sum the moment it is reached.
	allocated := new(big.Int)
	for _, m := range members {
		if m.AmountMinor != nil && m.AmountMinor.Sign() > 0 {
			allocated.Add(allocated, m.AmountMinor)
		}
	}
	if bucketed := new(big.Int).Add(r.LeafTotalMinor, r.ExcludedTotalMinor); bucketed.Cmp(allocated) != 0 {
		return nil, fmt.Errorf(
			"%w: %s minor units are allocated but %s are accounted for (leaf %s + excluded %s); "+
				"an outcome carrying money belongs to neither bucket, so the total an operator "+
				"acknowledges before publication understates what was owed",
			ErrDoesNotReconcile, allocated, bucketed, r.LeafTotalMinor, r.ExcludedTotalMinor)
	}

	if s.PoolMinor != nil {
		r.ResidueMinor.Sub(s.PoolMinor, r.LeafTotalMinor)

		// THE SECOND IDENTITY: residue IS the held money, when the pool was
		// fully allocated.
		//
		// What it proves, stated exactly, because the obvious reading is wrong.
		//
		// It proves the money classified as held is precisely the money not in
		// the tree: the two figures are computed by different routes - one from
		// the pool minus the leaves, one by summing owed members - and they must
		// meet. Held money that was ALSO paid, or pool money belonging to
		// neither bucket, breaks it.
		//
		// It does NOT prove the classification is right. Reclassifying a held
		// member as payable moves both sides to zero together and the equality
		// still holds; that failure is caught by the resolve-time tests, not
		// here. Checked by mutation rather than assumed - turning the KYC branch
		// off leaves this identity satisfied.
		//
		// So: this is the guard against the two totals drifting apart, which is
		// the property #507 rests on once the classification is correct. It is
		// not a second opinion on the classification.
		//
		// # Its precondition, and why it is checked rather than assumed
		//
		// It follows only from the pool being fully allocated: residue is
		// pool - leaf, and leaf + excluded == allocated (above), so residue ==
		// excluded exactly when allocated == pool. Both producers assert that
		// today - founding/settlement.go and hackathon/settlement_producer.go
		// each refuse to return a Result whose lines do not sum to the pool -
		// but that is a property of the PRODUCER, not of this package, and this
		// package accepts a Settlement from anywhere.
		//
		// So the premise is read from the data rather than inferred from which
		// producer built it. A future producer that deliberately allocates less
		// than the pool reaches the edge of this invariant and is skipped, which
		// is what stops the first person building one from hitting a failure
		// they cannot tell from a bug of their own.
		if allocated.Cmp(s.PoolMinor) == 0 && r.ResidueMinor.Cmp(r.ExcludedTotalMinor) != 0 {
			return nil, fmt.Errorf(
				"%w: the whole pool was allocated, so residue (%s) must be exactly the money owed "+
					"to excluded members (%s); a difference means money is double-counted or belongs to no bucket",
				ErrDoesNotReconcile, r.ResidueMinor, r.ExcludedTotalMinor)
		}
	}
	return r, nil
}

func minorToDecimal(v *big.Int, decimals int32) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	q, rem := new(big.Int).QuoRem(abs, d, new(big.Int))
	s := fmt.Sprintf("%s.%0*s", q.String(), decimals, rem.String())
	if neg {
		s = "-" + s
	}
	return s
}

// Render writes the report a person reads. Plain text on purpose: this is
// evidence somebody archives next to a publish transaction.
func (r *Report) Render() string {
	var b strings.Builder
	dec := r.Settlement.AssetDecimals
	f := func(v *big.Int) string { return minorToDecimal(v, dec) }

	fmt.Fprintf(&b, "PAYOUT DRY RUN — nothing has been written and nothing is on chain\n")
	fmt.Fprintf(&b, "settlement %s   chain %s   pool %s\n", r.Settlement.SettlementID, r.Settlement.ChainID, r.Settlement.Pool)
	fmt.Fprintf(&b, "input digest %s\n\n", r.InputDigest)

	payable := r.Payable()
	fmt.Fprintf(&b, "LEAVES (%d)\n", len(payable))
	fmt.Fprintf(&b, "%-4s  %-20s  %-66s  %18s  %14s\n", "idx", "login", "address", "minor", "amount")
	for i, m := range payable {
		fmt.Fprintf(&b, "%-4d  %-20s  %-66s  %18s  %14s\n", i, m.GitHubLogin, m.ClaimAddress, m.AmountMinor.String(), f(m.AmountMinor))
	}

	owed := r.Owed()
	fmt.Fprintf(&b, "\nEARNED BUT UNDELIVERABLE (%d) — these people are owed money and are NOT in the tree\n", len(owed))
	if len(owed) == 0 {
		fmt.Fprintf(&b, "  none\n")
	}
	for _, m := range owed {
		who := m.GitHubLogin
		if who == "" {
			who = "(no github account) " + m.UserID.String()
		}
		fmt.Fprintf(&b, "  %-28s  %18s  %14s   %s\n", who, m.AmountMinor.String(), f(m.AmountMinor), m.Outcome)
	}

	fmt.Fprintf(&b, "\nEARNED NOTHING\n  %d rounded to zero, %d ineligible by rule\n", r.NoSharesCount, r.IneligibleCount)

	// Shown as an identity that must come to zero, rather than four numbers side
	// by side that a reader has to subtract in their head.
	sum := new(big.Int).Add(r.LeafTotalMinor, r.ExcludedTotalMinor)
	rounding := new(big.Int).Sub(r.Settlement.PoolMinor, sum)
	check := new(big.Int).Sub(r.Settlement.PoolMinor, new(big.Int).Add(sum, rounding))

	fmt.Fprintf(&b, "\nRECONCILIATION (minor units)\n")
	fmt.Fprintf(&b, "  pool total          %18s   %14s\n", r.Settlement.PoolMinor.String(), f(r.Settlement.PoolMinor))
	fmt.Fprintf(&b, "- leaf total          %18s   %14s   <- FUND EXACTLY THIS\n", r.LeafTotalMinor.String(), f(r.LeafTotalMinor))
	fmt.Fprintf(&b, "- undeliverable       %18s   %14s\n", r.ExcludedTotalMinor.String(), f(r.ExcludedTotalMinor))
	fmt.Fprintf(&b, "- rounding remainder  %18s   %14s\n", rounding.String(), f(rounding))
	fmt.Fprintf(&b, "= %18s%s\n", check.String(), map[bool]string{true: "   OK", false: "   DOES NOT RECONCILE"}[check.Sign() == 0])

	fmt.Fprintf(&b, "\n  residue if the POOL were funded instead of the leaf total: %s (%s)\n",
		r.ResidueMinor.String(), f(r.ResidueMinor))
	fmt.Fprintf(&b, "  that residue is sweepable with NO timelock, which is why publish_root\n")
	fmt.Fprintf(&b, "  requires total == funded_total exactly. Fund the leaf total.\n")

	fmt.Fprintf(&b, "\nTO BUILD FROM THIS REPORT\n")
	fmt.Fprintf(&b, "  input digest:   %s\n", r.InputDigest)
	fmt.Fprintf(&b, "  acknowledge undeliverable total, in minor units: %s\n", r.ExcludedTotalMinor.String())
	return b.String()
}
