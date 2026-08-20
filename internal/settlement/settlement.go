// Package settlement is the shape every producer of money writes, and the one
// place a fraction becomes an integer.
//
// # Why this is not inside a producer
//
// A settlement has two halves: deciding who is owed what, and dividing an
// integer pool among them. The first is producer-specific - founding reads
// shares and wave multipliers, a hackathon reads judged units and a curve. The
// second is identical for both and must stay identical, because it is the step
// that turns an exact rational into the integer that goes into a Merkle leaf.
//
// Expressed twice, the two divisions drift, and the drift is invisible: both
// produce plausible integers that sum to the pool, and the first symptom is two
// events that apportioned a tie differently with no record of why. This
// repository has already paid once for a rule expressed in two places, between
// the Go leaf builder and the Soroban contract.
//
// So: producers compute weights, this package divides. Nothing here knows what
// a wave, a bucket or a judge is.
//
// # The boundary with internal/payout
//
// internal/payout consumes settlements and produces trees. It should import
// this package and nothing else about producers - one adapter over this shape,
// not one adapter per producer. Centralising the rounding while leaving the
// shape per-producer would be the same split that allowed two tree paths to
// exist, moved one level up.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AssetDecimals is USDC's precision. Minor units throughout: 1 USDC is
// 1_000_000 minor units, and every amount that reaches a contract or a leaf is
// an integer count of those.
const AssetDecimals = 6

// ErrAllocationMismatch means the computed lines do not sum to the pool.
//
// Checked rather than assumed. Largest-remainder apportionment makes it
// unreachable, which is exactly why it is worth asserting: if the method is
// ever replaced by one that does not conserve the total, the failure is silent,
// and its first symptom is an escrow that cannot honour its own root.
var ErrAllocationMismatch = errors.New("allocated amounts do not sum to the pool")

// Line is one person's computed settlement.
//
// Weights are exact rationals and the payout is an exact integer. Nothing here
// is a float, and the transition between the two happens only in Apportion.
type Line struct {
	UserID uuid.UUID
	// RawWeight is what the producer's own ledger recorded, exact. Shares for
	// founding, units for a hackathon.
	RawWeight *big.Rat
	// Multiplier is whatever the producer applies on top, exact.
	Multiplier *big.Rat
	// EffectiveWeight is RawWeight x Multiplier, exact. Zero for anyone the
	// producer judged ineligible.
	EffectiveWeight *big.Rat
	// AmountMinor is what this person may claim, in exact minor units. This is
	// the number that goes into a Merkle leaf; nothing else here does.
	AmountMinor *big.Int
	// IneligibleReason records why somebody got nothing, which matters as much
	// as why somebody got something.
	IneligibleReason string
}

// Result is a whole computed settlement, before any money moves.
type Result struct {
	// SettlementID is set only by Persist. A dry run leaves it zero, which is
	// how a caller can tell a read-only result from a recorded one.
	SettlementID uuid.UUID
	// HackathonID is nil for producers that are not an event, such as founding.
	HackathonID *uuid.UUID
	// Pool is "contributor" or "maintainer". An event with both produces TWO
	// settlements, two roots, two escrows and two fundings - they are separate
	// settlements, not two halves of one.
	Pool string
	// ChainID is the chain this settlement pays on.
	ChainID       string
	PoolMinor     *big.Int
	AssetDecimals int32
	// TotalEffective is the divisor: the sum of eligible effective weights.
	TotalEffective *big.Rat
	Lines          []Line
}

// PayableLines returns only the lines with a strictly positive amount.
//
// This is what a Merkle tree is built from, and the filter is not cosmetic. A
// zero-amount leaf is permanently unclaimable - every contract rejects
// amount <= 0 - so it is dead weight in the tree, and worse, it permanently
// commits "this identity was in this event and received nothing" to a root that
// cannot be edited.
func (r *Result) PayableLines() []Line {
	out := make([]Line, 0, len(r.Lines))
	for _, l := range r.Lines {
		if l.AmountMinor != nil && l.AmountMinor.Sign() > 0 {
			out = append(out, l)
		}
	}
	return out
}

// TotalAllocatedMinor sums every line's amount.
func (r *Result) TotalAllocatedMinor() *big.Int {
	total := new(big.Int)
	for _, l := range r.Lines {
		if l.AmountMinor != nil {
			total.Add(total, l.AmountMinor)
		}
	}
	return total
}

// Apportion divides poolMinor across the lines in exact integer minor units, by
// the largest-remainder method.
//
// Each line's exact entitlement is pool x effective / total, a rational that is
// almost never a whole number of minor units. Every line takes the floor, which
// leaves a shortfall of strictly fewer minor units than there are eligible
// lines. Those leftover units are handed out one each, and **who receives them
// is a rule, not an accident**:
//
//  1. Largest fractional remainder first.
//  2. Ties broken by lowest user id.
//
// Rule 2 is arbitrary and deliberately so: what matters is that it is total,
// stable, and recomputable from stored rows months later. Exact ties are
// possible whenever two people hold identical weight, which with a small pool is
// common rather than exotic - so leaving the tie to map iteration order would
// make the settlement irreproducible, and a settlement nobody can recompute
// cannot answer a dispute.
//
// The stake is one minor unit, 0.000001 USDC. No reading of fairness is worth
// more than determinism at that size, which is why the simplest total rule wins
// over a cleverer one.
func Apportion(lines []Line, poolMinor *big.Int, total *big.Rat) error {
	poolRat := new(big.Rat).SetInt(poolMinor)

	type share struct {
		idx  int
		frac *big.Rat
	}
	var contenders []share
	allocated := new(big.Int)

	for i := range lines {
		lines[i].AmountMinor = new(big.Int)
		if lines[i].EffectiveWeight.Sign() <= 0 {
			continue
		}

		exact := new(big.Rat).Mul(poolRat, lines[i].EffectiveWeight)
		exact.Quo(exact, total)

		// Every value here is non-negative, so truncation is floor.
		floor := new(big.Int).Quo(exact.Num(), exact.Denom())
		lines[i].AmountMinor.Set(floor)
		allocated.Add(allocated, floor)

		frac := new(big.Rat).Sub(exact, new(big.Rat).SetInt(floor))
		contenders = append(contenders, share{idx: i, frac: frac})
	}

	leftover := new(big.Int).Sub(poolMinor, allocated)
	if leftover.Sign() < 0 {
		return fmt.Errorf("%w: floors alone exceeded the pool by %s", ErrAllocationMismatch, new(big.Int).Neg(leftover))
	}
	if leftover.Sign() == 0 {
		return nil
	}
	if !leftover.IsInt64() || leftover.Int64() > int64(len(contenders)) {
		return fmt.Errorf("%w: %s units left over across %d eligible lines",
			ErrAllocationMismatch, leftover, len(contenders))
	}

	sort.Slice(contenders, func(a, b int) bool {
		if c := contenders[a].frac.Cmp(contenders[b].frac); c != 0 {
			return c > 0 // larger remainder first
		}
		return lines[contenders[a].idx].UserID.String() < lines[contenders[b].idx].UserID.String()
	})

	one := big.NewInt(1)
	for i := int64(0); i < leftover.Int64(); i++ {
		lines[contenders[i].idx].AmountMinor.Add(lines[contenders[i].idx].AmountMinor, one)
	}
	return nil
}

// Persist records an approved settlement. **Call this only after a human has
// read the result**, whichever producer computed it.
//
// Separate from any producer's dry run so that computing a settlement and
// committing to one are different acts. The sum-to-pool invariant is re-checked
// here rather than trusted, because a Result can be constructed or mutated by a
// caller between the two.
func Persist(ctx context.Context, pool db.DBPool, res *Result) error {
	if res == nil {
		return errors.New("settlement.Persist: nil result")
	}
	if got := res.TotalAllocatedMinor(); got.Cmp(res.PoolMinor) != 0 {
		return fmt.Errorf("%w: allocated %s of %s minor units",
			ErrAllocationMismatch, got, res.PoolMinor)
	}
	poolKind := res.Pool
	if poolKind == "" {
		poolKind = "contributor"
	}

	unitValue := new(big.Rat).Quo(new(big.Rat).SetInt(res.PoolMinor), res.TotalEffective)

	if err := pool.QueryRow(ctx, `
INSERT INTO settlements
  (hackathon_id, pool, chain_id, pool_usdc, pool_minor, asset_decimals, total_weight, unit_value_usdc)
VALUES ($1, $2, NULLIF($3,''), $4::numeric, $5, $6, $7::numeric, $8::numeric)
RETURNING id
`,
		res.HackathonID,
		poolKind,
		res.ChainID,
		RatToDecimalString(new(big.Rat).SetFrac(res.PoolMinor, big.NewInt(1_000_000)), 6),
		res.PoolMinor.String(),
		res.AssetDecimals,
		RatToDecimalString(res.TotalEffective, 4),
		RatToDecimalString(new(big.Rat).Quo(unitValue, big.NewRat(1_000_000, 1)), 8),
	).Scan(&res.SettlementID); err != nil {
		return fmt.Errorf("settlement.Persist: insert settlement: %w", err)
	}

	for _, l := range res.Lines {
		if _, err := pool.Exec(ctx, `
INSERT INTO settlement_lines
  (settlement_id, user_id, raw_weight, multiplier, effective_weight, usdc_amount, amount_minor, ineligible_reason)
VALUES ($1, $2, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7, NULLIF($8, ''))
`,
			res.SettlementID, l.UserID,
			RatToDecimalString(l.RawWeight, 4),
			RatToDecimalString(l.Multiplier, 3),
			RatToDecimalString(l.EffectiveWeight, 7),
			RatToDecimalString(new(big.Rat).SetFrac(l.AmountMinor, big.NewInt(1_000_000)), 6),
			l.AmountMinor.String(),
			l.IneligibleReason,
		); err != nil {
			return fmt.Errorf("settlement.Persist: insert line: %w", err)
		}
	}
	return nil
}

// RatToDecimalString renders an exact rational for storage in a NUMERIC column.
//
// big.Rat.FloatString rounds to nearest at the requested precision, which is
// correct here because every value handed to it either fits exactly (weights,
// multipliers, amounts) or is a derived reporting figure that no arithmetic
// reads back.
func RatToDecimalString(v *big.Rat, places int) string {
	if v == nil {
		return "0"
	}
	return v.FloatString(places)
}
