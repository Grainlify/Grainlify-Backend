// Package chain is the chain-agnostic abstraction from the GrainHack
// on-chain spec §3.
//
// Nothing above this package knows which chain it is talking to. Four
// implementations will eventually sit behind this interface (Soroban,
// Starknet, Flare, Solana) and they must stay behaviourally identical, so the
// abstraction is built and proven first - against mocks - before any real
// chain exists. A flaw found here is cheap; found across four audited
// contracts it is four migrations.
//
// **This package never signs anything and never holds a key.** Every method
// that would move value returns an UnsignedTx for a separate signer service
// to validate, sign and submit. That is a deliberate custody boundary, not an
// implementation convenience.
package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Errors callers distinguish rather than string-match.
var (
	ErrUnknownChain     = errors.New("no adapter registered for that chain")
	ErrNotFunded        = errors.New("escrow is not funded to the required amount")
	ErrNotConfirmed     = errors.New("the transaction has not reached the required confirmation depth")
	ErrInvalidAddress   = errors.New("address is not valid for this chain")
	ErrPoolMismatch     = errors.New("amount does not match the escrowed pool")
	ErrCommitmentAbsent = errors.New("no such commitment on chain")
)

// Amount is a decimal with explicit precision, never a chain's native
// integer.
//
// On-chain spec §3.1: no chain-specific types leak upward. §9 also requires
// that all decimal arithmetic happens off-chain and only integer minor units
// are published, so the conversion to a chain's integer representation is the
// adapter's job and happens exactly once, at the boundary.
type Amount struct {
	// Minor is the value in the asset's smallest unit (e.g. 1_000_000 for
	// 1.0 USDC at 6 decimals). Held as big.Int because 64 bits is not
	// obviously enough forever and silent overflow on money is unacceptable.
	Minor *big.Int
	// Decimals is the asset's precision, carried with the value so an
	// amount is never ambiguous about what it means.
	Decimals int32
}

// NewAmount builds an Amount from minor units.
func NewAmount(minor int64, decimals int32) Amount {
	return Amount{Minor: big.NewInt(minor), Decimals: decimals}
}

// IsZero reports whether the amount is zero or unset.
func (a Amount) IsZero() bool { return a.Minor == nil || a.Minor.Sign() == 0 }

// Cmp compares two amounts of the same precision. Comparing different
// precisions is a bug rather than something to coerce silently, so it
// returns an error instead of guessing.
func (a Amount) Cmp(b Amount) (int, error) {
	if a.Decimals != b.Decimals {
		return 0, fmt.Errorf("chain.Amount: cannot compare %d-decimal with %d-decimal amounts", a.Decimals, b.Decimals)
	}
	if a.Minor == nil || b.Minor == nil {
		return 0, fmt.Errorf("chain.Amount: uninitialised amount")
	}
	return a.Minor.Cmp(b.Minor), nil
}

func (a Amount) String() string {
	if a.Minor == nil {
		return "0"
	}
	return a.Minor.String()
}

// AssetSpec identifies the asset a chain's pool is denominated in.
type AssetSpec struct {
	Symbol string `json:"symbol"`
	// Issuer/Contract are chain-shaped strings the adapter interprets; no
	// caller above the adapter parses them.
	Issuer   string `json:"issuer,omitempty"`
	Contract string `json:"contract,omitempty"`
	Decimals int32  `json:"decimals"`
}

// AddressFormatSpec describes what a valid address looks like, for UI hints.
// It is descriptive only - validation is always ValidateAddress, never a
// caller re-implementing the rule from these fields.
type AddressFormatSpec struct {
	Description string `json:"description"`
	Example     string `json:"example"`
	Pattern     string `json:"pattern,omitempty"`
}

// EscrowRef is opaque to callers (§3.1). It identifies one chain's escrow for
// one event; only the adapter interprets its contents.
type EscrowRef struct {
	ChainID     string `json:"chain_id"`
	HackathonID string `json:"hackathon_id"`
	// Contract is the deployed escrow address on that chain, when known.
	Contract string `json:"contract,omitempty"`
}

func (e EscrowRef) String() string { return e.ChainID + ":" + e.HackathonID }

// EscrowParams is what funding an escrow needs.
//
// The two pools are separate fields, never one total with accounting. §5.1
// requires it to be structurally impossible for a maintainer claim to draw on
// contributor funds, mirroring the off-chain separate-budget guarantee.
type EscrowParams struct {
	Ref             EscrowRef
	ContributorPool Amount
	MaintainerPool  Amount
	Asset           AssetSpec
	// ConfigHash is published with funding where the chain allows it in the
	// same transaction (§5.2).
	ConfigHash [32]byte
}

// UnsignedTx is a transaction built but not signed. The signer service
// validates the shape, signs and submits; this package never does.
type UnsignedTx struct {
	ChainID string `json:"chain_id"`
	// Kind names the operation, so a signer can enforce an allow-list on
	// shape rather than trusting whatever it is handed.
	Kind string `json:"kind"`
	// Payload is the chain's own serialised transaction envelope.
	Payload []byte `json:"payload"`
	// Summary is human-readable, for the signing approval step. A signer
	// approving an opaque blob is a signer approving anything.
	Summary string `json:"summary"`
}

// FundedState is what the chain says about an escrow's balances.
type FundedState struct {
	Exists          bool   `json:"exists"`
	ContributorPool Amount `json:"contributor_pool"`
	MaintainerPool  Amount `json:"maintainer_pool"`
	Confirmations   int    `json:"confirmations"`
}

// ClaimState is what the chain says about one Merkle leaf.
type ClaimState struct {
	Known   bool   `json:"known"`
	Claimed bool   `json:"claimed"`
	TxHash  string `json:"tx_hash,omitempty"`
}

// Commitment kinds recorded in chain_commitments.
const (
	CommitmentConfigHash = "config_hash"
	CommitmentDrawCommit = "draw_commit"
	CommitmentDrawReveal = "draw_reveal"
	CommitmentClaimRoot  = "claim_root"
)

// On-chain action states. §3.4: exactly three, never two - a confirmed action
// that later reorgs out returns to submitted and is retried rather than being
// silently trusted.
const (
	StateSubmitted = "submitted"
	StateConfirmed = "confirmed"
	StateFailed    = "failed"
)

// ChainAdapter is the one interface every chain implements (§3.1).
//
// Everything that would move value returns an UnsignedTx. Adding a chain must
// never touch the signing path, and must require no change above this
// interface - if a caller ever needs to branch on ChainID(), that is a design
// bug to surface rather than work around.
type ChainAdapter interface {
	ChainID() string
	NativeAsset() AssetSpec

	// Escrow
	BuildFundEscrow(ctx context.Context, p EscrowParams) (UnsignedTx, error)
	VerifyEscrowFunded(ctx context.Context, ref EscrowRef) (FundedState, error)

	// Commitments
	BuildPublishConfigHash(ctx context.Context, ref EscrowRef, hash [32]byte) (UnsignedTx, error)
	BuildCommitDrawSeed(ctx context.Context, ref EscrowRef, issueRef string, commit [32]byte) (UnsignedTx, error)
	BuildRevealDrawSeed(ctx context.Context, ref EscrowRef, issueRef string, seed []byte) (UnsignedTx, error)

	// Claims
	BuildPublishClaimRoot(ctx context.Context, ref EscrowRef, root [32]byte, total Amount) (UnsignedTx, error)
	BuildSweepUnclaimed(ctx context.Context, ref EscrowRef, dest string) (UnsignedTx, error)

	// Reading
	GetCommitment(ctx context.Context, ref EscrowRef, key string) ([32]byte, bool, error)
	GetClaimStatus(ctx context.Context, ref EscrowRef, leafHash [32]byte) (ClaimState, error)
	ConfirmationsFor(ctx context.Context, txHash string) (int, error)

	// Addresses
	ValidateAddress(addr string) error
	AddressFormat() AddressFormatSpec
}

// Registry resolves a chain id to its adapter.
type Registry struct {
	adapters map[string]ChainAdapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: map[string]ChainAdapter{}}
}

func (r *Registry) Register(a ChainAdapter) {
	r.adapters[strings.ToLower(a.ChainID())] = a
}

// For returns the adapter for a chain id.
func (r *Registry) For(chainID string) (ChainAdapter, error) {
	a, ok := r.adapters[strings.ToLower(chainID)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownChain, chainID)
	}
	return a, nil
}

// ChainIDs lists registered chains, for diagnostics.
func (r *Registry) ChainIDs() []string {
	out := make([]string, 0, len(r.adapters))
	for id := range r.adapters {
		out = append(out, id)
	}
	return out
}

// bigZero is a fresh zero, so callers never share a mutable big.Int.
func bigZero() *big.Int { return big.NewInt(0) }
