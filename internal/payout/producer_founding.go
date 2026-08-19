package payout

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/founding"
)

// FromFounding adapts a founding settlement into the neutral shape.
//
// This is the seam. Everything founding-specific stops here: pool_usdc and
// share_value_usdc are decimal USDC belonging to how founding computes a
// settlement, and band multipliers and wave assignment are how it decides who is
// owed what. None of that crosses into payout, which receives the answer and
// never the derivation.
//
// A hackathon settlement gets its own function beside this one, producing the
// same Settlement. If a second one cannot be expressed this way, widen
// Settlement - do not give the second producer its own tree path. Two tree paths
// disagree the first time anybody compares them, which this repository has
// already paid for once between the Go leaf builder and the Soroban contract.
func FromFounding(res *founding.Result, chainID string) (Settlement, error) {
	if res == nil {
		return Settlement{}, fmt.Errorf("payout.FromFounding: nil result")
	}
	if res.SettlementID.String() == "00000000-0000-0000-0000-000000000000" {
		// A dry-run Result carries no settlement id by design, and a tree must
		// be anchored to a persisted settlement or nothing can be reconciled
		// against it later.
		return Settlement{}, fmt.Errorf("payout.FromFounding: settlement has not been persisted")
	}

	ents := make([]Entitlement, 0, len(res.Lines))
	for _, l := range res.Lines {
		amt := l.AmountMinor
		if amt == nil {
			amt = new(big.Int)
		}
		ents = append(ents, Entitlement{
			UserID:           l.UserID,
			AmountMinor:      new(big.Int).Set(amt),
			IneligibleReason: l.IneligibleReason,
		})
	}

	pool := res.PoolMinor
	if pool == nil {
		pool = new(big.Int)
	}
	return Settlement{
		SettlementID:  res.SettlementID,
		ChainID:       chainID,
		PoolMinor:     new(big.Int).Set(pool),
		AssetDecimals: res.AssetDecimals,
		// The founding pool is the contributor pool. A maintainer pool is a
		// different event with its own settlement and its own root.
		Pool:         "contributor",
		Entitlements: ents,
	}, nil
}

// LoadSettlement reconstructs the neutral shape from a persisted founding
// settlement.
//
// Lives here rather than in cmd/payout because constructing a Settlement is this
// package's job: a command that assembled one itself would be a second place
// that decides what an entitlement is, and the two would drift.
func LoadSettlement(ctx context.Context, pool db.DBPool, settlementID uuid.UUID, chainID string) (Settlement, error) {
	var poolMinor int64
	var decimals int32
	if err := pool.QueryRow(ctx, `
		SELECT pool_minor, asset_decimals FROM founding_settlements WHERE id = $1`,
		settlementID).Scan(&poolMinor, &decimals); err != nil {
		return Settlement{}, fmt.Errorf("payout.LoadSettlement: no settlement %s: %w", settlementID, err)
	}

	rows, err := pool.Query(ctx, `
		SELECT user_id, amount_minor, COALESCE(ineligible_reason, '')
		FROM `+settlementLinesTable+` WHERE settlement_id = $1 ORDER BY user_id`, settlementID)
	if err != nil {
		return Settlement{}, fmt.Errorf("payout.LoadSettlement: lines: %w", err)
	}
	defer rows.Close()

	var ents []Entitlement
	for rows.Next() {
		var e Entitlement
		var amt int64
		if err := rows.Scan(&e.UserID, &amt, &e.IneligibleReason); err != nil {
			return Settlement{}, err
		}
		e.AmountMinor = big.NewInt(amt)
		ents = append(ents, e)
	}
	if err := rows.Err(); err != nil {
		return Settlement{}, err
	}
	if len(ents) == 0 {
		return Settlement{}, fmt.Errorf("payout.LoadSettlement: settlement %s has no lines", settlementID)
	}

	return Settlement{
		SettlementID:  settlementID,
		ChainID:       chainID,
		PoolMinor:     big.NewInt(poolMinor),
		AssetDecimals: decimals,
		Pool:          "contributor",
		Entitlements:  ents,
	}, nil
}
