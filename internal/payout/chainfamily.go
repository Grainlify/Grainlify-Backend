package payout

import (
	"context"
	"errors"
	"fmt"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

// validateStored re-checks an address read back from contributor_addresses,
// using the family recorded on that row.
//
// # Why a read needs validating at all
//
// Because this is the last point before the value is committed to a Merkle
// leaf, and a leaf is permanent. The column has a CHECK, but a value crossing
// back out of the database is still a value crossing a boundary.
//
// # Why the family must be respected here
//
// payoutaddr.Validate left-pads to 64 hex characters. Applied to a 40-hex EVM
// address it returns a DIFFERENT, valid Aptos address - so the unconditional
// call this replaces would, the first time an EVM settlement ran, publish a
// root committing everybody's money to addresses nobody controls. Nothing would
// error. See Grainlify-Backend#548.
//
// An unrecognised family returns an error, which Resolve treats as "no address"
// rather than guessing - a person recorded as having no payout address is
// recoverable, and money sent to a padded address is not.
func validateStored(family, addr string) (string, error) {
	switch family {
	case FamilyAptos:
		return payoutaddr.Validate(addr)
	case FamilyEVM:
		return payoutaddr.ValidateEVM(addr)
	default:
		return "", fmt.Errorf("%w: stored address %q has family %q",
			ErrChainFamilyUnknown, addr, family)
	}
}

// Chain families, and the lookup that decides which validator an address gets.
//
// # Why a family and not a chain list in the code
//
// The alternative is a switch on chain_id somewhere in a handler, which is a
// list of chains that has to be found and edited every time one is added, and
// whose default case is the interesting one. Adding a chain should be a row,
// not a deploy - that is what chain_configs is for - so the row states its own
// family and the code asks.
const (
	FamilyAptos = "aptos"
	FamilyEVM   = "evm"
)

// Three errors, because they are three different operator mistakes, following
// the rule already set by ErrChainNotConfigured and ErrChainConfigIncomplete in
// this package: one name per cause, never a name that could cover two.
var (
	// ErrChainNotEnabled is a configured chain that is switched off. Distinct
	// from "no row": the remedy is a flag, not a seed, and telling somebody to
	// seed a chain that already exists sends them to write a duplicate.
	ErrChainNotEnabled = errors.New("chain_not_enabled")

	// ErrChainFamilyUnknown is a row whose family is absent or unrecognised.
	// A half-finished seed, in the same family as ErrChainConfigIncomplete.
	ErrChainFamilyUnknown = errors.New("chain_family_unknown")
)

// ChainFamilyFor returns the family of an ENABLED chain, or an error.
//
// # Why enablement is checked here rather than by the caller
//
// This function exists to answer "how do I validate an address for this chain",
// and every caller asking that is about to accept a payout destination. A
// disabled chain is one we cannot pay on, so accepting an address for it stores
// a destination that will never be used, under a chain_id whose meaning may
// still change before it is switched on.
//
// Refusing here also closes the free-text gap in #548 directly: chain_id stops
// being "any non-empty string" and becomes "a chain we actually have".
//
// There is deliberately no default. A chain with no row, a disabled chain and a
// chain whose family nobody set are three different things, and none of them is
// "probably Aptos" - assuming that is how #548 happened.
func ChainFamilyFor(ctx context.Context, pool db.DBPool, chainID string) (string, error) {
	var (
		family  *string
		enabled bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT family, enabled FROM chain_configs WHERE chain_id = $1`, chainID).
		Scan(&family, &enabled); err != nil {
		return "", fmt.Errorf("%w: %q has no row in chain_configs", ErrChainNotConfigured, chainID)
	}
	if !enabled {
		return "", fmt.Errorf("%w: %q is configured but not enabled", ErrChainNotEnabled, chainID)
	}
	if family == nil || *family == "" {
		return "", fmt.Errorf("%w: %q has no family set", ErrChainFamilyUnknown, chainID)
	}
	switch *family {
	case FamilyAptos, FamilyEVM:
		return *family, nil
	default:
		return "", fmt.Errorf("%w: %q declares family %q, which nothing knows how to validate",
			ErrChainFamilyUnknown, chainID, *family)
	}
}
