package chain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrPartialFailure is returned when an operation succeeded on some of an
// event's chains and not others.
//
// §3.2: partial failure is normal and must be handled. If Stellar's escrow
// confirms and Flare's does not, the event does not go live - it stays in its
// current phase with an alert naming the chain. Never let an event proceed
// with a subset of its chains funded, because that is a promise made to some
// contributors and not others.
var ErrPartialFailure = errors.New("operation did not succeed on every active chain")

// Pool is one chain's sub-pool for one event, as stored in
// hackathon_chain_pools.
type Pool struct {
	HackathonID     string
	ChainID         string
	ContributorPool Amount
	MaintainerPool  Amount
	EscrowRef       EscrowRef
	FundingTx       string
	State           string
	// MinConfirmations comes from chain_configs, per chain (§3.3).
	MinConfirmations int
}

// PoolResult is the outcome of dispatching one operation to one chain.
type PoolResult struct {
	ChainID string
	Tx      UnsignedTx
	Err     error
}

// DispatchResult aggregates a fan-out across every active chain.
type DispatchResult struct {
	Results []PoolResult
}

// Failed returns the chains that errored, sorted for stable messages.
func (d DispatchResult) Failed() []string {
	var out []string
	for _, r := range d.Results {
		if r.Err != nil {
			out = append(out, r.ChainID)
		}
	}
	sort.Strings(out)
	return out
}

// Err reports partial failure, naming the chains. Returns nil only when every
// chain succeeded - "all or nothing" is the point, so a caller that ignores
// this proceeds partially funded.
func (d DispatchResult) Err() error {
	failed := d.Failed()
	if len(failed) == 0 {
		return nil
	}
	var detail []string
	for _, r := range d.Results {
		if r.Err != nil {
			detail = append(detail, fmt.Sprintf("%s: %v", r.ChainID, r.Err))
		}
	}
	sort.Strings(detail)
	return fmt.Errorf("%w: %s", ErrPartialFailure, strings.Join(detail, "; "))
}

// Dispatch runs the same operation against every pool's adapter.
//
// There is no per-chain branching here and no path that assumes exactly one
// chain (§3.2) - a single-chain event is this loop with one entry. Every pool
// is attempted even after one fails, so an operator sees every problem at
// once rather than fixing them one deploy at a time.
func Dispatch(
	ctx context.Context,
	reg *Registry,
	pools []Pool,
	op func(context.Context, ChainAdapter, Pool) (UnsignedTx, error),
) DispatchResult {
	var out DispatchResult
	for _, p := range pools {
		adapter, err := reg.For(p.ChainID)
		if err != nil {
			out.Results = append(out.Results, PoolResult{ChainID: p.ChainID, Err: err})
			continue
		}
		tx, err := op(ctx, adapter, p)
		out.Results = append(out.Results, PoolResult{ChainID: p.ChainID, Tx: tx, Err: err})
	}
	return out
}

// FundAllEscrows builds the funding transaction for every active chain
// (§7: the issue_prep -> live transition).
func FundAllEscrows(ctx context.Context, reg *Registry, pools []Pool, configHash [32]byte) DispatchResult {
	return Dispatch(ctx, reg, pools, func(ctx context.Context, a ChainAdapter, p Pool) (UnsignedTx, error) {
		return a.BuildFundEscrow(ctx, EscrowParams{
			Ref:             p.EscrowRef,
			ContributorPool: p.ContributorPool,
			MaintainerPool:  p.MaintainerPool,
			Asset:           a.NativeAsset(),
			ConfigHash:      configHash,
		})
	})
}

// PublishConfigHashAll publishes the same snapshot hash to every active chain.
// §5.2: the rules are event-wide; only the pools are per chain.
func PublishConfigHashAll(ctx context.Context, reg *Registry, pools []Pool, hash [32]byte) DispatchResult {
	return Dispatch(ctx, reg, pools, func(ctx context.Context, a ChainAdapter, p Pool) (UnsignedTx, error) {
		return a.BuildPublishConfigHash(ctx, p.EscrowRef, hash)
	})
}

// EscrowReadiness is one chain's funding status against what it owes.
type EscrowReadiness struct {
	ChainID string
	Ready   bool
	Reason  string
	State   FundedState
}

// VerifyAllEscrowsFunded checks every chain holds at least what it owes, at
// the configured confirmation depth.
//
// Re-read from the chain every time rather than trusting a stored
// "confirmed" (§3.4): a confirmed transaction that later reorgs out must be
// caught here, which only works if this never short-circuits on remembered
// state.
func VerifyAllEscrowsFunded(ctx context.Context, reg *Registry, pools []Pool) ([]EscrowReadiness, error) {
	out := make([]EscrowReadiness, 0, len(pools))
	allReady := true

	for _, p := range pools {
		r := EscrowReadiness{ChainID: p.ChainID}
		adapter, err := reg.For(p.ChainID)
		if err != nil {
			r.Reason = err.Error()
			out = append(out, r)
			allReady = false
			continue
		}
		st, err := adapter.VerifyEscrowFunded(ctx, p.EscrowRef)
		if err != nil {
			r.Reason = fmt.Sprintf("could not read escrow: %v", err)
			out = append(out, r)
			allReady = false
			continue
		}
		r.State = st

		switch {
		case !st.Exists:
			r.Reason = "no escrow found on chain"
		case st.Confirmations < p.MinConfirmations:
			r.Reason = fmt.Sprintf("funding has %d confirmation(s), needs %d", st.Confirmations, p.MinConfirmations)
		default:
			cCmp, cErr := st.ContributorPool.Cmp(p.ContributorPool)
			mCmp, mErr := st.MaintainerPool.Cmp(p.MaintainerPool)
			switch {
			case cErr != nil || mErr != nil:
				r.Reason = "escrow balance precision does not match the recorded pool"
			case cCmp < 0:
				r.Reason = fmt.Sprintf("contributor pool holds %s, owes %s", st.ContributorPool, p.ContributorPool)
			case mCmp < 0:
				r.Reason = fmt.Sprintf("maintainer pool holds %s, owes %s", st.MaintainerPool, p.MaintainerPool)
			default:
				r.Ready = true
			}
		}
		if !r.Ready {
			allReady = false
		}
		out = append(out, r)
	}

	if !allReady {
		var names []string
		for _, r := range out {
			if !r.Ready {
				names = append(names, fmt.Sprintf("%s (%s)", r.ChainID, r.Reason))
			}
		}
		sort.Strings(names)
		return out, fmt.Errorf("%w: %s", ErrPartialFailure, strings.Join(names, "; "))
	}
	return out, nil
}

// GuardDrawCommitConfirmed enforces §5.3's refuse-to-draw rule.
//
// If the commit transaction has not confirmed before the window closes, the
// draw does not run - the window is extended and an alert raised. Running it
// anyway silently converts a verifiable draw into a trusted one, which is
// worse than a late draw because nobody can tell from the outside.
//
// Reads the chain rather than the database on purpose: the question is
// whether the commitment is actually on chain, and a row saying so is exactly
// what a reorg makes untrue.
func GuardDrawCommitConfirmed(
	ctx context.Context,
	reg *Registry,
	pool Pool,
	issueRef string,
	commitTxHash string,
) error {
	adapter, err := reg.For(pool.ChainID)
	if err != nil {
		return err
	}

	if commitTxHash == "" {
		return fmt.Errorf("%w: no draw-seed commit has been submitted for issue %s on %s",
			ErrNotConfirmed, issueRef, pool.ChainID)
	}

	conf, err := adapter.ConfirmationsFor(ctx, commitTxHash)
	if err != nil {
		return fmt.Errorf("could not read confirmations for the draw commit on %s: %w", pool.ChainID, err)
	}
	if conf < pool.MinConfirmations {
		return fmt.Errorf("%w: draw-seed commit for issue %s on %s has %d confirmation(s), needs %d - extend the window rather than drawing",
			ErrNotConfirmed, issueRef, pool.ChainID, conf, pool.MinConfirmations)
	}

	if _, ok, err := adapter.GetCommitment(ctx, pool.EscrowRef, CommitmentKey(CommitmentDrawCommit, issueRef)); err != nil {
		return fmt.Errorf("could not read the draw commit on %s: %w", pool.ChainID, err)
	} else if !ok {
		return fmt.Errorf("%w: no draw-seed commit on %s for issue %s", ErrCommitmentAbsent, pool.ChainID, issueRef)
	}
	return nil
}
