package founding

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
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

// Line and Result live in internal/settlement now.
//
// Type ALIASES, not new definitions: founding.Result and settlement.Result are
// the same type, so anything already written against founding.Result - including
// the other session's payout.FromFounding - keeps compiling with no edit. That
// is the whole reason this is an alias, and it is why the adapter collapse can
// be a separate, clean change on main rather than a cross-session edit now.
type (
	Line   = settlement.Line
	Result = settlement.Result
)

// AssetDecimals and ErrAllocationMismatch are re-exported for the same reason.
const AssetDecimals = settlement.AssetDecimals

var ErrAllocationMismatch = settlement.ErrAllocationMismatch

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
		total.Add(total, lines[i].EffectiveWeight)
	}
	if total.Sign() <= 0 {
		return nil, ErrNoShares
	}

	if err := settlement.Apportion(lines, poolMinor, total); err != nil {
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
			id              uuid.UUID
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
			RawWeight:       shr,
			Multiplier:      mult,
			EffectiveWeight: new(big.Rat),
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
		lines[i].EffectiveWeight.Mul(lines[i].RawWeight, lines[i].Multiplier)
	}

	sort.Slice(lines, func(a, b int) bool {
		return lines[a].UserID.String() < lines[b].UserID.String()
	})
	return lines, nil
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
