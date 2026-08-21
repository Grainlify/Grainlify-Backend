package payout

import (
	"context"
	"errors"
	"fmt"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Two errors, because they are two different operator mistakes.
//
// "This chain has no row" is a missing seed. "This chain has a row with a NULL
// contract address" is a half-finished one. Collapsing them tells whoever is
// paged that something is wrong with the chain and not which half to go and fix.
// Same rule this codebase applies to nonces and wallet responses: one name per
// cause, never a name that could cover two.
var (
	ErrChainNotConfigured    = errors.New("chain_not_configured")
	ErrChainConfigIncomplete = errors.New("chain_config_incomplete")
)

// ChainConfig is everything a client needs to reach the contract, so that it
// needs no constants of its own.
//
// # Why this exists at all
//
// The frontend had to hardcode a module address, because nothing served one.
// That is fine until the first redeploy and guaranteed to break at testnet ->
// mainnet, which is a redeploy to a different address by definition. The rule
// this type enforces: **wherever a client would otherwise need a constant, serve
// it.**
//
// Network is a LABEL - "testnet" - and never a URL. See migration 085.
type ChainConfig struct {
	ChainID             string
	ContractAddress     string
	ExplorerURLTemplate string
	Network             string
	AssetSymbol         string
	AssetDecimals       int32
}

// ChainConfigFor reads one chain's configuration, or fails.
//
// # No empty-string fallback, in any field
//
// A missing contract address scanned into a string is "", which a client will
// concatenate into "::escrow::claim" and submit against address 0x - a
// transaction that is not a Grainlify escrow and not anything else either. The
// precedent is the decimals lookup this replaces, which already refused to
// default: a zero there renders 250000 minor units as "250000.00 USDC".
//
// So every field a client would otherwise hardcode is required, and absence is
// an error naming the field.
func ChainConfigFor(ctx context.Context, pool db.DBPool, chainID string) (ChainConfig, error) {
	var (
		c                                   ChainConfig
		contract, explorer, network, symbol *string
		decimals                            *int32
	)
	err := pool.QueryRow(ctx, `
		SELECT chain_id, contract_address, explorer_url_template, network,
		       asset->>'symbol', (asset->>'decimals')::int
		FROM chain_configs WHERE chain_id = $1`, chainID).
		Scan(&c.ChainID, &contract, &explorer, &network, &symbol, &decimals)
	if err != nil {
		return ChainConfig{}, fmt.Errorf("%w: %q has no row in chain_configs; "+
			"seed it before serving claims on this chain", ErrChainNotConfigured, chainID)
	}

	// Reported together rather than one at a time, so a half-seeded row takes one
	// round trip to fix instead of four.
	var missing []string
	if contract == nil || *contract == "" {
		missing = append(missing, "contract_address")
	}
	if explorer == nil || *explorer == "" {
		missing = append(missing, "explorer_url_template")
	}
	if network == nil || *network == "" {
		missing = append(missing, "network")
	}
	if symbol == nil || *symbol == "" {
		missing = append(missing, "asset.symbol")
	}
	if decimals == nil {
		missing = append(missing, "asset.decimals")
	}
	if len(missing) > 0 {
		return ChainConfig{}, fmt.Errorf("%w: chain %q is configured but missing %v. "+
			"A client would otherwise hardcode these, which is the failure serving them prevents",
			ErrChainConfigIncomplete, chainID, missing)
	}

	c.ContractAddress = *contract
	c.ExplorerURLTemplate = *explorer
	c.Network = *network
	c.AssetSymbol = *symbol
	c.AssetDecimals = *decimals
	return c, nil
}
