package payout

import (
	"fmt"
	"math/big"

	"github.com/jagadeesh/grainlify/backend/internal/founding"
)

// FromFounding adapts a founding settlement into the neutral shape.
//
// This is the seam. Everything founding-specific stops here: pool_usdc and
// unit_value_usdc are decimal USDC belonging to how founding computes a
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
