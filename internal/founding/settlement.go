package founding

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ErrNoShares means nobody earned anything, so there is nothing to divide.
// Dividing by zero here would produce an infinite share value and a
// settlement that pays the first person the whole pool.
var ErrNoShares = errors.New("no shares were earned, so there is nothing to settle")

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
type Line struct {
	UserID           uuid.UUID
	RawShares        float64
	Multiplier       float64
	EffectiveShares  float64
	USDCAmount       float64
	IneligibleReason string
}

// Result is a whole computed settlement, before any money moves.
type Result struct {
	SettlementID   uuid.UUID
	PoolUSDC       float64
	TotalShares    float64
	ShareValueUSDC float64
	Lines          []Line
}

// Compute works out every member's payout and records it. **It moves no
// money and releases nothing.**
//
//	share_value = founding_pool / total_effective_shares
//	payout      = your_effective_shares × share_value
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
// Deterministic: lines are ordered by user id so a recomputation months later
// produces byte-identical output, which is what makes a dispute answerable.
func Compute(ctx context.Context, pool db.DBPool, hackathonID *uuid.UUID, cfg map[string]string) (*Result, error) {
	poolUSDC := atofOr(cfg["founding_pool_usdc"], 3000)

	rows, err := pool.Query(ctx, `
SELECT m.user_id, m.multiplier, COALESCE(sum(s.shares), 0)::float8
FROM founding_members m
LEFT JOIN founding_shares s ON s.user_id = m.user_id
GROUP BY m.user_id, m.multiplier
ORDER BY m.user_id
`)
	if err != nil {
		return nil, fmt.Errorf("founding.Compute: load members: %w", err)
	}
	defer rows.Close()

	var lines []Line
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.UserID, &l.Multiplier, &l.RawShares); err != nil {
			return nil, fmt.Errorf("founding.Compute: scan: %w", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	total := 0.0
	for i := range lines {
		ok, reason, err := Eligible(ctx, pool, lines[i].UserID, cfg)
		if err != nil {
			return nil, err
		}
		if !ok {
			lines[i].IneligibleReason = reason
			lines[i].EffectiveShares = 0
			continue
		}
		lines[i].EffectiveShares = lines[i].RawShares * lines[i].Multiplier
		total += lines[i].EffectiveShares
	}
	if total <= 0 {
		return nil, ErrNoShares
	}

	shareValue := poolUSDC / total
	for i := range lines {
		lines[i].USDCAmount = round6(lines[i].EffectiveShares * shareValue)
	}
	sort.Slice(lines, func(a, b int) bool {
		return lines[a].UserID.String() < lines[b].UserID.String()
	})

	res := &Result{PoolUSDC: poolUSDC, TotalShares: total, ShareValueUSDC: shareValue, Lines: lines}
	if err := persist(ctx, pool, hackathonID, res); err != nil {
		return nil, err
	}
	return res, nil
}

func persist(ctx context.Context, pool db.DBPool, hackathonID *uuid.UUID, res *Result) error {
	if err := pool.QueryRow(ctx, `
INSERT INTO founding_settlements (hackathon_id, pool_usdc, total_shares, share_value_usdc)
VALUES ($1, $2, $3, $4)
RETURNING id
`, hackathonID, res.PoolUSDC, res.TotalShares, res.ShareValueUSDC).Scan(&res.SettlementID); err != nil {
		return fmt.Errorf("founding.Compute: insert settlement: %w", err)
	}
	for _, l := range res.Lines {
		if _, err := pool.Exec(ctx, `
INSERT INTO founding_settlement_lines
  (settlement_id, user_id, raw_shares, multiplier, effective_shares, usdc_amount, ineligible_reason)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
`, res.SettlementID, l.UserID, l.RawShares, l.Multiplier, l.EffectiveShares, l.USDCAmount, l.IneligibleReason); err != nil {
			return fmt.Errorf("founding.Compute: insert line: %w", err)
		}
	}
	return nil
}

func round6(v float64) float64 {
	const f = 1e6
	if v >= 0 {
		return float64(int64(v*f+0.5)) / f
	}
	return float64(int64(v*f-0.5)) / f
}
