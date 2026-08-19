package hackathon

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// The hackathon's producer of settlement rows.
//
// A hackathon decides who is owed what; internal/settlement divides the pool.
// This file is only the first half, and it deliberately stops at handing over
// weights.
//
// # An event produces one settlement PER POOL
//
// A hackathon carries a contributor pool and a maintainer pool on the same row.
// They are two settlements, two Merkle roots, two on-chain escrows and two
// fundings - not two halves of one. Whoever funds an event must fund both, and
// an event whose maintainer escrow is unfunded will publish its contributor
// pool and stall on the other half with nothing saying why.
//
// The idempotency key is UNIQUE (hackathon_id, pool) for exactly this reason.
//
// # Where the weights come from, and where they must NOT come from
//
// Weight is units x curve_multiplier, both read as text and parsed as exact
// rationals. That is the same shape founding uses - raw weight times a
// multiplier - and it is why settlement.Line has those three fields.
//
// **Do not use hackathon_verdicts.payout_amount here, and do not use
// effective_units.** Both are already-computed answers:
//
//   - payout_amount is what ComputePayout allocated. Feeding it in would round
//     twice - once through ComputePayout's own division and floor, once through
//     Apportion - and two roundings of the same money cannot be reconciled
//     against a single published root.
//   - effective_units is units x curve_multiplier computed in float64
//     (PayoutPlan is float64 throughout) and stored back. Reading it would put a
//     binary-float value into a divisor that is otherwise exact, for no gain,
//     when the two exact columns it was derived from are sitting beside it.
//
// So: the integer and the multiplier go in, and Apportion is the only place a
// fraction becomes an integer. If you came here looking for payout_amount, that
// is the column you want to leave alone.
//
// # What this depends on having happened
//
// units and curve_multiplier are written by CloseAppealsAndRecompute, which runs
// on the closed -> settled transition. Before that they are NULL and there is
// nothing to settle - which is correct, not a gap: the amounts are not decided
// until appeals close.

// ErrNothingToSettle means there is no money to divide or nobody to divide it
// among. An ordinary outcome, not a failure: an event where every submission was
// rejected settles to nothing and should say so rather than error.
var ErrNothingToSettle = errors.New("nothing to settle")

// SettlementFor computes one pool's settlement for a hackathon, writing nothing.
//
// Returns ErrNothingToSettle when no verdict carries a positive weight, which is
// an ordinary outcome for an event where every submission was rejected, not a
// failure.
func SettlementFor(
	ctx context.Context,
	pool db.DBPool,
	hackathonID uuid.UUID,
	poolKind string,
	chainID string,
) (*settlement.Result, error) {
	if poolKind != "contributor" && poolKind != "maintainer" {
		return nil, fmt.Errorf("hackathon.SettlementFor: unknown pool %q", poolKind)
	}

	poolMinor, err := poolMinorFor(ctx, pool, hackathonID, poolKind)
	if err != nil {
		return nil, err
	}
	if poolMinor.Sign() <= 0 {
		return nil, fmt.Errorf("%w: the %s pool is zero or unset", ErrNothingToSettle, poolKind)
	}

	lines, err := loadVerdictLines(ctx, pool, hackathonID, poolKind)
	if err != nil {
		return nil, err
	}

	total := new(big.Rat)
	for i := range lines {
		total.Add(total, lines[i].EffectiveWeight)
	}
	if total.Sign() <= 0 {
		return nil, fmt.Errorf("%w: no submission carried a positive weight", ErrNothingToSettle)
	}

	if err := settlement.Apportion(lines, poolMinor, total); err != nil {
		return nil, fmt.Errorf("hackathon.SettlementFor: %w", err)
	}

	hid := hackathonID
	res := &settlement.Result{
		HackathonID:    &hid,
		Pool:           poolKind,
		ChainID:        chainID,
		PoolMinor:      poolMinor,
		AssetDecimals:  settlement.AssetDecimals,
		TotalEffective: total,
		Lines:          lines,
	}

	// The invariant, checked rather than assumed - the same assertion founding
	// makes, for the same reason: if the apportionment is ever replaced by a
	// method that does not conserve the total, the failure is silent and its
	// first symptom is an escrow that cannot honour its own root.
	if got := res.TotalAllocatedMinor(); got.Cmp(poolMinor) != 0 {
		return nil, fmt.Errorf("%w: allocated %s of %s minor units",
			settlement.ErrAllocationMismatch, got, poolMinor)
	}
	return res, nil
}

// loadVerdictLines reads every judged submission as an exact weight.
//
// Ordered by user id so a recomputation months later produces the same rows in
// the same order, which is what makes a settlement answerable in a dispute.
//
// Both numeric values are read as text and parsed as exact rationals rather than
// scanned into a float. NUMERIC in Postgres is exact and big.Rat is exact;
// float64 in between is the one lossy step, and it is the step this whole shape
// exists to avoid.
func loadVerdictLines(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, poolKind string) ([]settlement.Line, error) {
	// Maintainer payouts are computed by a different mechanism entirely
	// (hackathon_maintainer_payouts, with its own holdback schedule) and are
	// not derived from verdicts. Refusing here is honest; returning contributor
	// rows under a maintainer label would not be.
	if poolKind == "maintainer" {
		return nil, fmt.Errorf("hackathon.loadVerdictLines: the maintainer pool is not settled from verdicts - see hackathon_maintainer_payouts")
	}

	rows, err := pool.Query(ctx, `
SELECT v.user_id,
       COALESCE(v.units, 0)::text,
       COALESCE(v.curve_multiplier, 1)::text
FROM hackathon_verdicts v
WHERE v.hackathon_id = $1
  AND v.user_id IS NOT NULL
  AND v.final_bucket IS NOT NULL
  AND v.final_bucket <> 'rejected'
ORDER BY v.user_id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.loadVerdictLines: %w", err)
	}
	defer rows.Close()

	// One person can merge several PRs in one event, and each is its own
	// verdict. They are summed into a single line, because a settlement pays a
	// person once: two lines for one user would violate the
	// (settlement_id, user_id) uniqueness and, worse, would put two leaves in
	// the tree for one identity.
	byUser := map[uuid.UUID]*settlement.Line{}
	var order []uuid.UUID
	for rows.Next() {
		var uid uuid.UUID
		var unitsStr, multStr string
		if err := rows.Scan(&uid, &unitsStr, &multStr); err != nil {
			return nil, fmt.Errorf("hackathon.loadVerdictLines: scan: %w", err)
		}
		units, ok := new(big.Rat).SetString(unitsStr)
		if !ok {
			return nil, fmt.Errorf("hackathon.loadVerdictLines: units %q for %s is not a number", unitsStr, uid)
		}
		mult, ok := new(big.Rat).SetString(multStr)
		if !ok {
			return nil, fmt.Errorf("hackathon.loadVerdictLines: curve multiplier %q for %s is not a number", multStr, uid)
		}
		eff := new(big.Rat).Mul(units, mult)

		if l, seen := byUser[uid]; seen {
			l.RawWeight.Add(l.RawWeight, units)
			l.EffectiveWeight.Add(l.EffectiveWeight, eff)
			continue
		}
		byUser[uid] = &settlement.Line{
			UserID:          uid,
			RawWeight:       units,
			Multiplier:      mult,
			EffectiveWeight: eff,
		}
		order = append(order, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hackathon.loadVerdictLines: iterate: %w", err)
	}

	out := make([]settlement.Line, 0, len(order))
	for _, uid := range order {
		out = append(out, *byUser[uid])
	}
	return out, nil
}

// poolMinorFor reads the configured pool and converts it to exact minor units.
//
// Parsed as a rational rather than a float so "3000.50" is exactly 3_000_500_000
// minor units and not a value that depends on binary floating point.
func poolMinorFor(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, poolKind string) (*big.Int, error) {
	col := "contributor_prize_pool"
	if poolKind == "maintainer" {
		col = "maintainer_prize_pool"
	}
	var raw *string
	if err := pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s::text FROM hackathons WHERE id = $1`, col), hackathonID).Scan(&raw); err != nil {
		return nil, fmt.Errorf("hackathon.poolMinorFor: read %s: %w", col, err)
	}
	if raw == nil || *raw == "" {
		return new(big.Int), nil
	}
	r, ok := new(big.Rat).SetString(*raw)
	if !ok {
		return nil, fmt.Errorf("hackathon.poolMinorFor: %s %q is not a number", col, *raw)
	}
	minor := new(big.Rat).Mul(r, new(big.Rat).SetInt64(1_000_000))
	if !minor.IsInt() {
		return nil, fmt.Errorf("hackathon.poolMinorFor: %s %q is finer than one minor unit", col, *raw)
	}
	return new(big.Int).Set(minor.Num()), nil
}

// ExistingSettlementID reports whether this event and pool have already been
// settled, and if so which settlement it was.
//
// Lives here rather than in the handler that displays it, because
// internal/founding's TestSettlementFiguresNeverReachAPresentationLayer forbids
// a settlement table being read from anything that renders to a person - and it
// is right to. A handler that queries settlements is one edit away from
// rendering a per-person figure out of it; a handler that calls this gets back a
// uuid and cannot.
func ExistingSettlementID(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, poolKind string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`SELECT id FROM settlements WHERE hackathon_id = $1 AND pool = $2`,
		hackathonID, poolKind).Scan(&id)
	if err != nil {
		return nil, nil //nolint:nilerr // absence is the ordinary answer, not a failure
	}
	return &id, nil
}
