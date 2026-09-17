package keeperhubrail

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The mandatory pre-dispatch check.
//
// # Why mandatory, not a flag
//
// Simulate is EVM-only, but that is not a live constraint on this rail:
// checkEvent already refuses any non-EVM chain before a release ever reaches
// this point (ErrNotEVMChain), so every leg simulateLegs ever sees is already,
// by construction, on a chain simulation supports. The EVM-only limitation
// changes nothing about whether this should be optional.
//
// The round trip is once per release - one simulate call per leg, once, when
// an admin explicitly releases a run - not a cost paid on every poll or every
// read. That is a vanishingly small price next to a wrong payout.
//
// An optional safety check on a money path is a check that eventually gets
// skipped: under time pressure, "just this once", and then it is not
// exercising anything. So there is no field, no config key and no request
// parameter that disables this. Every leg a release would send is simulated,
// every time, or the release does not happen.
//
// # Why an unreachable simulator fails exactly like a leg that would revert
//
// A leg preflight could not check is not a leg preflight assumes is fine. It is
// treated with the same severity as a leg that reports it would revert:
// nothing is claimed, nothing is dispatched, either way. The two causes are
// kept distinguishable in the error returned (ErrPreflightUnavailable vs
// ErrPreflightWouldRevert) and in what is logged, so an operator reading a
// refusal knows immediately whether to fix the rail's own tooling (the org
// key, KeeperHub's endpoint, chain configuration) or fix the leg (the address,
// the amount, the settlement that produced it) - but that distinction is
// diagnostic only. Nothing here ever proceeds to Dispatch on the strength of
// "we could not tell".

// LegPreflight is what SimulateTransfer established for one leg, immediately
// before release would have sent it.
//
// Persisted onto the leg's attempt row (keeperhub_dispatch_attempt_legs) once
// the release proceeds, so RunView can show, after the fact, that this
// specific leg was checked and what was found - not merely that the feature
// exists.
type LegPreflight struct {
	LegID uuid.UUID `json:"leg_id"`

	// Safe mirrors TransferSimulation.Safe(): Success && !WouldRevert &&
	// Error == "". Every leg release actually claims and dispatches is Safe
	// by construction - Release refuses the whole batch otherwise - so a
	// persisted row will always read Safe here. Kept anyway rather than
	// dropped, matching Raw below: this is a record of what was checked, not
	// only a gate.
	Safe        bool   `json:"safe"`
	WouldRevert bool   `json:"would_revert"`
	Error       string `json:"error,omitempty"`

	// Unavailable is true when SimulateTransfer itself could not be run for
	// this leg - the org key, the network, KeeperHub's endpoint. Distinct from
	// WouldRevert: this leg's fate on chain is unknown, not unfavourable.
	Unavailable       bool   `json:"unavailable"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	CheckedAt time.Time `json:"checked_at"`

	// Raw is KeeperHub's simulation response, kept whole for the same reason
	// TransferSimulation keeps it: the fields it returns vary by action and a
	// dropped one is one nobody can get back. Excluded from this struct's own
	// JSON (the API response embeds it separately where useful) and stored as
	// JSONB alongside the attempt leg.
	Raw json.RawMessage `json:"-"`
}

// preflightChain is the chain configuration simulateLegs needs, read once per
// call rather than once per leg.
type preflightChain struct {
	evmChainID   int64
	tokenAddress string
	decimals     int32
}

// loadPreflightChain reads what simulation needs about the run's chain.
//
// A chain missing a token_address or asset decimals fails closed as
// ErrPreflightUnavailable rather than guessing at a native transfer: this
// rail has never paid anything but an ERC-20, and an absent value here is a
// configuration gap, not evidence the chain pays natively.
func (s *Service) loadPreflightChain(ctx context.Context, chainID string) (*preflightChain, error) {
	var pc preflightChain
	var token *string
	var decimals *int32
	err := s.Pool.QueryRow(ctx, `
		SELECT evm_chain_id, asset->>'token_address', (asset->>'decimals')::int
		FROM chain_configs WHERE chain_id = $1`, chainID).Scan(&pc.evmChainID, &token, &decimals)
	if err != nil {
		return nil, fmt.Errorf("%w: load chain %q for preflight: %v", ErrPreflightUnavailable, chainID, err)
	}
	if token == nil || strings.TrimSpace(*token) == "" {
		return nil, fmt.Errorf("%w: chain %q has no token_address configured to simulate against", ErrPreflightUnavailable, chainID)
	}
	if decimals == nil {
		return nil, fmt.Errorf("%w: chain %q has no asset decimals configured", ErrPreflightUnavailable, chainID)
	}
	pc.tokenAddress = *token
	pc.decimals = *decimals
	return &pc, nil
}

// simulateLegs is the mandatory preflight: every leg previewRun found
// sendable is simulated, in order, before claimPreviewedLegs is ever called.
//
// Returns as soon as the first unsafe or unreachable leg is found, with every
// LegPreflight established up to and including that leg - so a caller can see
// exactly how far the check got before it stopped release.
func (s *Service) simulateLegs(ctx context.Context, chainID string, legs []sendableLeg) ([]LegPreflight, error) {
	pc, err := s.loadPreflightChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	chainIDStr := strconv.FormatInt(pc.evmChainID, 10)

	out := make([]LegPreflight, 0, len(legs))
	for _, l := range legs {
		human, herr := minorToHuman(l.AmountMinor, pc.decimals)
		if herr != nil {
			checked := LegPreflight{LegID: l.ID, Unavailable: true, UnavailableReason: herr.Error(), CheckedAt: time.Now()}
			out = append(out, checked)
			return out, fmt.Errorf("%w: leg %s: %v", ErrPreflightUnavailable, l.ID, herr)
		}

		sim, simErr := s.Rail.SimulateTransfer(ctx, chainIDStr, l.Address, human, pc.tokenAddress)
		checked := LegPreflight{LegID: l.ID, CheckedAt: time.Now()}
		if simErr != nil {
			checked.Unavailable = true
			checked.UnavailableReason = simErr.Error()
			out = append(out, checked)
			return out, fmt.Errorf("%w: leg %s: %v", ErrPreflightUnavailable, l.ID, simErr)
		}
		checked.Safe = sim.Safe()
		checked.WouldRevert = sim.WouldRevert
		checked.Error = sim.Error
		checked.Raw = sim.Raw
		out = append(out, checked)
		if !checked.Safe {
			return out, fmt.Errorf("%w: leg %s to %s: wouldRevert=%v error=%q",
				ErrPreflightWouldRevert, l.ID, l.Address, checked.WouldRevert, checked.Error)
		}
	}
	return out, nil
}

// minorToHuman converts exact integer minor units to the human-readable
// decimal string SimulateTransfer's own contract requires - the one place in
// this preflight that is not minor units, matching keeperhub.Client's
// documented convention exactly. Computed with big.Int throughout: a payout
// amount is money, and this must never round through a float.
func minorToHuman(amountMinor string, decimals int32) (string, error) {
	n, ok := new(big.Int).SetString(amountMinor, 10)
	if !ok {
		return "", fmt.Errorf("amount %q is not an integer", amountMinor)
	}
	if decimals <= 0 {
		return n.String(), nil
	}
	neg := n.Sign() < 0
	s := new(big.Int).Abs(n).String()
	for len(s) <= int(decimals) {
		s = "0" + s
	}
	cut := len(s) - int(decimals)
	intPart, fracPart := s[:cut], strings.TrimRight(s[cut:], "0")
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}
