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
	if s.PoolMinor != nil {
		r.ResidueMinor.Sub(s.PoolMinor, r.LeafTotalMinor)
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
