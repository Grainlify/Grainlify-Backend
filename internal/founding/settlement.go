package founding

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ErrNoShares means nobody earned anything, so there is nothing to divide.
// Dividing by zero here would produce an infinite share value and a
// settlement that pays the first person the whole pool.
var ErrNoShares = errors.New("no shares were earned, so there is nothing to settle")

// ErrEmptyPool means the configured pool is zero or negative. Settling a zero
// pool is not a payout of nothing, it is a misconfiguration - every line would
// compute to zero and the resulting root would commit 38 people to claiming
// nothing, permanently.
var ErrEmptyPool = errors.New("the founding pool is zero or negative")

// ErrAllocationMismatch means the computed lines do not sum to the pool. It is
// unreachable by construction and checked anyway: this is the one invariant
// whose violation cannot be detected after a root is published.
var ErrAllocationMismatch = errors.New("allocated amounts do not sum to the pool")

// AssetDecimals is USDC's precision. Minor units throughout: 1 USDC is
// 1_000_000 minor units.
//
// §9 of the on-chain spec requires all decimal arithmetic to happen off-chain
// with only integers published, and a Merkle leaf commits to an exact integer.
// So the boundary between "decimal" and "integer" is here, in this file, and
// nowhere further downstream.
const AssetDecimals = 6

// Eligible reports whether a user may receive a share, and why not if they
// may not.
//
// Following the social accounts is an eligibility gate worth zero shares
// (§3). Paying for a follow pays real money for a free, reversible, botable
// action; requiring one gets the same follows, creates no liability, and
// cannot be farmed - there is nothing to collect.
//
// **Checked at settlement, not when shares were earned.** Somebody who
// follows, collects, and unfollows before the event ends is not a community
// member, and checking at earn time would pay them anyway.
//
// Known limitation, deliberately not papered over: this re-reads the stored
// approval, which is a screenshot an admin accepted. It is not a live check
// against GitHub, Telegram or LinkedIn. A genuine re-verification needs API
// integration that does not exist - GitHub could support it, Telegram and
// LinkedIn effectively cannot - so what this actually guarantees is that the
// approval has not been revoked, not that the follow still exists.
func Eligible(ctx context.Context, pool db.DBPool, userID uuid.UUID, cfg map[string]string) (bool, string, error) {
	// One definition, shared with AssignWave - see internal/founding/gate.go.
	// Entry and payment are the same rule, and expressed twice they drift
	// exactly when somebody flips the switch.
	ok, reason, err := approvedForFoundingPool(ctx, pool, userID, cfg)
	if err != nil {
		return false, "", fmt.Errorf("founding.Eligible: %w", err)
	}
	if ok {
		return true, "", nil
	}
	// Settlement's wording says "at settlement", because that is when this
	// runs and the distinction matters in a dispute: the approval was absent
	// at the moment the pool was shared out, whatever was true earlier.
	return false, reason + " at settlement", nil
}

// Line is one member's computed settlement.
//
// Shares are exact rationals and the payout is an exact integer. Nothing here
// is a float: a float64 cannot represent most USDC cents, so a rounded set of
// float lines is not guaranteed to sum to the pool - and the contract refuses
// a root larger than the escrow, so drift upward will not publish while drift
// downward strands dust in escrow permanently.
type Line struct {
	UserID uuid.UUID
	// RawShares as recorded in the ledger, exact.
	RawShares *big.Rat
	// Multiplier captured at wave assignment, exact.
	Multiplier *big.Rat
	// EffectiveShares is RawShares x Multiplier, exact. Zero for anyone
	// ineligible.
	EffectiveShares *big.Rat
	// AmountMinor is what this member may claim, in exact minor units. This
	// is the number that goes into a Merkle leaf; nothing else here does.
	AmountMinor *big.Int
	// IneligibleReason records why somebody got nothing, which matters as
	// much as why somebody got something.
	IneligibleReason string
}

// Result is a whole computed settlement, before any money moves.
type Result struct {
	// SettlementID is set only by Persist. A dry run leaves it zero, which is
	// how a caller can tell a read-only result from a recorded one.
	SettlementID  uuid.UUID
	PoolMinor     *big.Int
	AssetDecimals int32
	// TotalEffective is the divisor: the sum of eligible effective shares.
	TotalEffective *big.Rat
	Lines          []Line
}

// PayableLines returns only the lines with a strictly positive amount.
//
// This is what a Merkle tree is built from, and the filter is not cosmetic.
// A zero-amount leaf is permanently unclaimable - every contract rejects
// amount <= 0 - so it is dead weight in the tree, and worse, it permanently
// commits "this identity was in this event and received nothing" to a root
// that cannot be edited.
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

// DryRun works out every member's payout and returns it, **writing nothing.**
//
//	share_value = founding_pool / total_effective_shares
//	payout      = your_effective_shares x share_value
//
// Identical arithmetic to GrainHack's unit_value, and identical in the
// property that matters: nobody can compute their payout in advance, because
// it depends on everyone else's shares, which nobody knows until this runs.
//
// Ineligible members are computed at zero and **excluded from the divisor**.
// Leaving them in would silently redistribute their share to nobody -
// shrinking everyone's payout and leaving part of the pool unassigned, which
// is the one outcome that cannot be explained to anybody afterwards.
//
// **Read-only, and that is the point.** This is the output a human reads
// before anything touches a chain, and a function that recorded a settlement
// every time it was run would make exercising that judgement cost a row -
// which in practice pressures whoever is reading it into running it once and
// trusting the result. Persist is separate and is called only after approval.
//
// Deterministic: lines are ordered by user id so a recomputation months later
// produces byte-identical output, which is what makes a dispute answerable.
func DryRun(ctx context.Context, pool db.DBPool, cfg map[string]string) (*Result, error) {
	poolMinor, err := poolMinorFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	if poolMinor.Sign() <= 0 {
		return nil, ErrEmptyPool
	}

	lines, err := loadLines(ctx, pool, cfg)
	if err != nil {
		return nil, err
	}

	total := new(big.Rat)
	for i := range lines {
		total.Add(total, lines[i].EffectiveShares)
	}
	if total.Sign() <= 0 {
		return nil, ErrNoShares
	}

	if err := apportion(lines, poolMinor, total); err != nil {
		return nil, err
	}

	res := &Result{
		PoolMinor:      poolMinor,
		AssetDecimals:  AssetDecimals,
		TotalEffective: total,
		Lines:          lines,
	}

	// The invariant, checked rather than assumed. Largest-remainder
	// apportionment makes this unreachable, which is exactly why it is worth
	// asserting: if the method is ever changed for one that does not conserve
	// the total, the failure is silent, and its first symptom is an escrow
	// that cannot honour its own root.
	if got := res.TotalAllocatedMinor(); got.Cmp(poolMinor) != 0 {
		return nil, fmt.Errorf("%w: allocated %s of %s minor units",
			ErrAllocationMismatch, got, poolMinor)
	}
	return res, nil
}

// loadLines reads every member with their exact shares and multiplier.
//
// Both values are read as text and parsed as exact rationals rather than
// scanned into a float. NUMERIC in Postgres is exact and big.Rat is exact;
// float64 in between is the one lossy step, and it is the step this whole
// change exists to remove.
func loadLines(ctx context.Context, pool db.DBPool, cfg map[string]string) ([]Line, error) {
	rows, err := pool.Query(ctx, `
SELECT m.user_id, m.multiplier::text, COALESCE(sum(s.shares), 0)::text
FROM founding_members m
LEFT JOIN founding_shares s ON s.user_id = m.user_id
GROUP BY m.user_id, m.multiplier
ORDER BY m.user_id
`)
	if err != nil {
		return nil, fmt.Errorf("founding.DryRun: load members: %w", err)
	}
	defer rows.Close()

	var lines []Line
	for rows.Next() {
		var (
			id            uuid.UUID
			multStr, shrStr string
		)
		if err := rows.Scan(&id, &multStr, &shrStr); err != nil {
			return nil, fmt.Errorf("founding.DryRun: scan: %w", err)
		}
		mult, ok := new(big.Rat).SetString(multStr)
		if !ok {
			return nil, fmt.Errorf("founding.DryRun: multiplier %q for %s is not a number", multStr, id)
		}
		shr, ok := new(big.Rat).SetString(shrStr)
		if !ok {
			return nil, fmt.Errorf("founding.DryRun: shares %q for %s is not a number", shrStr, id)
		}
		lines = append(lines, Line{
			UserID:          id,
			RawShares:       shr,
			Multiplier:      mult,
			EffectiveShares: new(big.Rat),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range lines {
		ok, reason, err := Eligible(ctx, pool, lines[i].UserID, cfg)
		if err != nil {
			return nil, err
		}
		if !ok {
			lines[i].IneligibleReason = reason
			continue // EffectiveShares stays zero
		}
		lines[i].EffectiveShares.Mul(lines[i].RawShares, lines[i].Multiplier)
	}

	sort.Slice(lines, func(a, b int) bool {
		return lines[a].UserID.String() < lines[b].UserID.String()
	})
	return lines, nil
}

// apportion divides poolMinor across the lines in exact integer minor units,
// by the largest-remainder method.
//
// Each line's exact entitlement is pool x effective / total, a rational that
// is almost never a whole number of minor units. Every line takes the floor,
// which leaves a shortfall of strictly fewer minor units than there are
// eligible lines. Those leftover units are handed out one each, and **who
// receives them is a rule, not an accident**:
//
//	1. Largest fractional remainder first.
//	2. Ties broken by lowest user id.
//
// Rule 2 is arbitrary and deliberately so: what matters is that it is total,
// stable, and recomputable from stored rows months later. Exact ties are
// possible whenever two members hold identical shares in the same wave, which
// with a small pool is common rather than exotic - so leaving the tie to map
// iteration order would make the settlement irreproducible, and a settlement
// nobody can recompute cannot answer a dispute.
//
// The stake is one minor unit, 0.000001 USDC. No reading of fairness is worth
// more than determinism at that size, which is why the simplest total rule
// wins over a cleverer one.
func apportion(lines []Line, poolMinor *big.Int, total *big.Rat) error {
	poolRat := new(big.Rat).SetInt(poolMinor)

	type share struct {
		idx  int
		frac *big.Rat
	}
	var contenders []share
	allocated := new(big.Int)

	for i := range lines {
		lines[i].AmountMinor = new(big.Int)
		if lines[i].EffectiveShares.Sign() <= 0 {
			continue
		}

		exact := new(big.Rat).Mul(poolRat, lines[i].EffectiveShares)
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

// poolMinorFromConfig reads the configured pool and converts it to exact minor
// units.
//
// Parsed as a rational rather than a float so "3000.50" is exactly 3_000_500_000
// and not a value 0.0000001 away from it. A configured pool with more precision
// than the asset can express is a configuration error and is refused rather
// than rounded: rounding it would mean the pool actually settled is not the
// pool that was announced.
func poolMinorFromConfig(cfg map[string]string) (*big.Int, error) {
	raw := cfg["founding_pool_usdc"]
	if raw == "" {
		raw = "3000"
	}
	amount, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, fmt.Errorf("founding: pool %q is not a number", raw)
	}

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(AssetDecimals), nil)
	scaled := new(big.Rat).Mul(amount, new(big.Rat).SetInt(scale))
	if !scaled.IsInt() {
		return nil, fmt.Errorf("founding: pool %q has more precision than %d decimals can express", raw, AssetDecimals)
	}
	return new(big.Int).Set(scaled.Num()), nil
}

// Persist records an approved settlement. **Call this only after a human has
// read the DryRun output.**
//
// Separate from DryRun so that computing a settlement and committing to one
// are distinct acts. It re-checks the invariant before writing anything: a
// Result can be constructed or mutated by a caller, and the sum-to-pool
// property is the one thing that must hold at the moment of recording, not
// merely at the moment of computation.
func Persist(ctx context.Context, pool db.DBPool, hackathonID *uuid.UUID, res *Result) error {
	if res == nil {
		return errors.New("founding.Persist: nil result")
	}
	if got := res.TotalAllocatedMinor(); got.Cmp(res.PoolMinor) != 0 {
		return fmt.Errorf("%w: allocated %s of %s minor units",
			ErrAllocationMismatch, got, res.PoolMinor)
	}

	shareValue := new(big.Rat).Quo(new(big.Rat).SetInt(res.PoolMinor), res.TotalEffective)

	if err := pool.QueryRow(ctx, `
INSERT INTO founding_settlements
  (hackathon_id, pool_usdc, pool_minor, asset_decimals, total_shares, share_value_usdc)
VALUES ($1, $2::numeric, $3, $4, $5::numeric, $6::numeric)
RETURNING id
`,
		hackathonID,
		ratToDecimalString(new(big.Rat).SetFrac(res.PoolMinor, big.NewInt(1_000_000)), 6),
		res.PoolMinor.String(),
		res.AssetDecimals,
		ratToDecimalString(res.TotalEffective, 4),
		ratToDecimalString(new(big.Rat).Quo(shareValue, big.NewRat(1_000_000, 1)), 8),
	).Scan(&res.SettlementID); err != nil {
		return fmt.Errorf("founding.Persist: insert settlement: %w", err)
	}

	for _, l := range res.Lines {
		if _, err := pool.Exec(ctx, `
INSERT INTO founding_settlement_lines
  (settlement_id, user_id, raw_shares, multiplier, effective_shares, usdc_amount, amount_minor, ineligible_reason)
VALUES ($1, $2, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7, NULLIF($8, ''))
`,
			res.SettlementID, l.UserID,
			ratToDecimalString(l.RawShares, 4),
			ratToDecimalString(l.Multiplier, 3),
			ratToDecimalString(l.EffectiveShares, 7),
			ratToDecimalString(new(big.Rat).SetFrac(l.AmountMinor, big.NewInt(1_000_000)), 6),
			l.AmountMinor.String(),
			l.IneligibleReason,
		); err != nil {
			return fmt.Errorf("founding.Persist: insert line: %w", err)
		}
	}
	return nil
}

// ratToDecimalString renders an exact rational at a fixed number of decimal
// places, for a NUMERIC column.
//
// big.Rat.FloatString rounds to nearest at the requested precision, which is
// correct here because every value handed to it either fits exactly (shares,
// multipliers, amounts) or is a derived reporting figure (share value) that no
// arithmetic reads back.
func ratToDecimalString(v *big.Rat, places int) string {
	if v == nil {
		return "0"
	}
	return v.FloatString(places)
}
