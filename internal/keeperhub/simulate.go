package keeperhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Simulation, and an honest account of what it does and does not cover.
//
// # Simulate does not exist on the webhook path
//
// There is no way to dry-run a workflow execution. Simulation lives on the
// DIRECT-EXECUTION calls - execute_transfer and execute_contract_call - which
// are a different mechanism from the workflow this client fires.
//
// So SimulateTransfer is a per-leg pre-flight, not a rehearsal of the run. It
// answers "would a transfer to this address, of this amount, on this chain,
// revert right now" for ONE leg. It cannot tell you the workflow will iterate
// correctly, and a run whose every leg simulates cleanly can still fail partway
// - which is exactly why the leg table exists and why only unpaid legs are ever
// re-sent.
//
// Stated plainly so nobody treats a clean simulation as permission to stop
// checking results.
//
// # EVM only
//
// Simulation is rejected for Solana chain ids before the API call. This rail is
// Base, so that is not a limitation here, but it is a reason this method must
// not be presented as a general safety net.
//
// Spends the ORG key: simulation signs and broadcasts nothing.

// TransferSimulation is the outcome of a dry-run transfer.
type TransferSimulation struct {
	// Success is whether the simulation itself ran.
	Success bool `json:"success"`

	// WouldRevert is the answer that matters. A simulation can succeed - the
	// call was made, the node answered - while reporting that the transfer
	// would revert. Reading only Success is how that gets missed, so both are
	// surfaced and Safe() requires them to agree.
	WouldRevert bool `json:"wouldRevert"`

	Error string `json:"error"`

	// Raw keeps the whole response, because the fields KeeperHub returns here
	// vary by action and a dropped field is one nobody can get back later.
	Raw json.RawMessage `json:"-"`
}

// Safe reports a leg that is clear to send.
//
// Both conditions, deliberately: the documented rule is to continue only after
// success=true AND wouldRevert=false, and an error string must be empty. A
// helper that checked one of the three would read as thorough and be wrong.
func (s TransferSimulation) Safe() bool {
	return s.Success && !s.WouldRevert && s.Error == ""
}

// SimulateTransfer dry-runs a single ERC-20 or native transfer.
//
// amount is in HUMAN-READABLE units, which is the one place in this package
// that is not minor units - the direct-execution API defines it that way, and
// converting it here silently would be worse than naming it. The caller
// converts, and the conversion is the caller's to get right.
//
// tokenAddress is the ERC-20 contract; empty means a native transfer.
func (c *Client) SimulateTransfer(ctx context.Context, chainID, toAddress, amount, tokenAddress string) (TransferSimulation, error) {
	if chainID == "" || toAddress == "" || amount == "" {
		return TransferSimulation{}, fmt.Errorf("keeperhub: chain id, destination and amount are all required")
	}
	args := map[string]any{
		"chain_id":   chainID,
		"to_address": toAddress,
		"amount":     amount,
		// A JSON boolean, never the string "true": the API rejects a stringified
		// value, and the failure mode of getting this wrong is a call that
		// BROADCASTS instead of simulating.
		"simulate": true,
	}
	if tokenAddress != "" {
		args["token_address"] = tokenAddress
	}
	// No idempotency key. The parameter exists, but a simulation moves nothing,
	// and sending one would mean a later *real* transfer with the same key is
	// deduped against a dry run and never broadcast.

	raw, err := c.mcpCall(ctx, "execute_transfer", args)
	if err != nil {
		// KeeperHub reports a transfer that WOULD REVERT as a tool error - an
		// HTTP 400 with isError set - even though its own text says "this 400
		// describes the transaction, not your request". Treated as a plain
		// error, the one answer this call exists to give was discarded: the
		// preflight filed the leg as "unavailable", the release answered 502
		// as though KeeperHub were down, and ErrPreflightWouldRevert was
		// unreachable for any real revert.
		//
		// Recovered only when the payload is unmistakably a simulation result.
		// Anything else - an auth failure, a malformed call, a body that does
		// not parse - stays an error, so this can only ever turn an error into
		// a refusal, never into permission to send.
		if sim, ok := simulationFromToolError(err); ok {
			return sim, nil
		}
		return TransferSimulation{}, err
	}
	var out TransferSimulation
	if err := json.Unmarshal(raw, &out); err != nil {
		return TransferSimulation{Raw: raw},
			fmt.Errorf("keeperhub: decode simulation: %w", err)
	}
	out.Raw = raw
	return out, nil
}

// simulationFromToolError recovers a simulation result from an isError payload.
//
// The text is "API call failed: 400 Bad Request - {json}" followed by prose, so
// the JSON is decoded as the first complete value after the first brace. It is
// accepted only with status "simulated" and wouldRevert true: a refusal is the
// only answer worth recovering, and requiring it means no reading of an error
// can ever produce a Safe() simulation.
func simulationFromToolError(err error) (TransferSimulation, bool) {
	var te *ToolError
	if !errors.As(err, &te) {
		return TransferSimulation{}, false
	}
	i := strings.IndexByte(te.Text, '{')
	if i < 0 {
		return TransferSimulation{}, false
	}
	var raw json.RawMessage
	if json.NewDecoder(strings.NewReader(te.Text[i:])).Decode(&raw) != nil {
		return TransferSimulation{}, false
	}
	var probe struct {
		Status      string `json:"status"`
		WouldRevert bool   `json:"wouldRevert"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Status != "simulated" || !probe.WouldRevert {
		return TransferSimulation{}, false
	}
	var sim TransferSimulation
	if json.Unmarshal(raw, &sim) != nil {
		return TransferSimulation{}, false
	}
	sim.Raw = raw
	return sim, true
}
