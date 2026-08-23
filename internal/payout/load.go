package payout

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// LoadSettlement reconstructs the neutral shape from any persisted settlement.
//
// Deliberately not in producer_founding.go, where it used to live. A settlement
// row is not founding's - migration 000082 renamed these tables precisely so
// they would stop being founding's - and a loader filed under one producer's
// name is a claim about its scope that this function does not honour. It reads
// a row by id; it does not know or care which producer wrote it.
//
// # Why the pool is read rather than assumed
//
// This function used to end with a hardcoded `Pool: "contributor"`, carried
// over from when the only settlements were founding's. The column has existed
// since 000082 and Persist has always written it; the loader simply threw it
// away.
//
// For a hackathon contributor pool the constant was accidentally right. For a
// maintainer pool it was silently wrong, and wrong in the worst available way:
// Build would have produced a complete, internally consistent Merkle tree with
// every leaf hashed against the contributor pool. Nothing errors. The tree is
// valid. It pays the wrong pool.
//
// THE ACKNOWLEDGEMENT GATE COULD NOT HAVE CAUGHT IT. InputDigest hashes
// s.Pool, so the digest appears to bind the pool - but dry-run and build both
// obtain the pool from this function, so both would have hashed the same
// constant, agreed, and passed. An invariant whose two sides are computed from
// one hardcoded value cannot test that value; it can only confirm the constant
// equals itself.
//
// The fix is that the pool is now a FACT read from the authoritative row, not
// a constant. Both sides still read it from that one row, and that is correct -
// the row is the authority, and two independent reads would only be two ways to
// disagree with it. What changed is what is being read, not how many readers
// there are.
//
// # chainID is an assertion, not an input
//
// The caller passes the chain it believes this settlement pays on. When the row
// records one, the row wins and a conflicting argument is refused rather than
// preferred: chain_id is hashed into the digest and decides which escrow is
// funded, so quietly choosing either side of a disagreement is how a tree gets
// built for one chain and funded on another.
//
// An empty stored chain_id falls back to the argument. Rows persisted before
// 000082 added the column carry NULL, and refusing those would break founding's
// existing sequence to fix a problem it does not have.
func LoadSettlement(ctx context.Context, pool db.DBPool, settlementID uuid.UUID, chainID string) (Settlement, error) {
	var poolMinor int64
	var decimals int32
	var poolKindStored, storedChain string
	if err := pool.QueryRow(ctx, `
		SELECT pool_minor, asset_decimals, pool, COALESCE(chain_id, '')
		FROM settlements WHERE id = $1`,
		settlementID).Scan(&poolMinor, &decimals, &poolKindStored, &storedChain); err != nil {
		return Settlement{}, fmt.Errorf("payout.LoadSettlement: no settlement %s: %w", settlementID, err)
	}

	// Checked here rather than trusted to the CHECK constraint. The constraint
	// guards writes; this guards a value about to be hashed into every leaf,
	// and the two are not the same moment.
	if poolKindStored != "contributor" && poolKindStored != "maintainer" {
		return Settlement{}, fmt.Errorf(
			"payout.LoadSettlement: settlement %s records pool %q, which is not a pool this can build a tree for",
			settlementID, poolKindStored)
	}

	effectiveChain := storedChain
	switch {
	case storedChain == "":
		effectiveChain = chainID
	case chainID != "" && chainID != storedChain:
		return Settlement{}, fmt.Errorf(
			"payout.LoadSettlement: settlement %s is recorded on chain %q but %q was requested; "+
				"the chain is hashed into the digest and decides which escrow is funded, so this is refused rather than resolved",
			settlementID, storedChain, chainID)
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
		ChainID:       effectiveChain,
		PoolMinor:     big.NewInt(poolMinor),
		AssetDecimals: decimals,
		Pool:          poolKindStored,
		Entitlements:  ents,
	}, nil
}
