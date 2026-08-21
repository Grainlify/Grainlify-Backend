// Package sponsor pays a contributor's gas so somebody who has never held APT
// can collect a payout.
//
// # Why this exists
//
// It did not, for weeks, while reading as though it did. A milestone showed a
// sponsored claim - first transaction ever, zero gas paid - and the fee payer
// was a script on a laptop. See Grainlify-Backend#537 and the traps entry
// "A milestone that read as a deployment".
//
// # Protecting the SPONSOR account, not just the escrow
//
// Checking that a transaction is a well-formed claim against an escrow we
// published protects the escrow and does nothing for us, because **a failed
// transaction still costs the fee payer gas**. A correctly-shaped claim for a
// leaf already claimed aborts on chain and we pay for it, in a loop.
//
// Four defences, which fail in different directions on purpose:
//
//  0. simulate before signing   catches every deterministic abort, costs nothing
//  1. read claim state          the common case, named precisely for the person
//  2. rate limit per user       bounds the check-to-submit race AND a bug in 0/1
//  3. balance floor + alarm     catches what nobody anticipated
//
// None is droppable. In particular (2) survives trusting (0) and (1): a defence
// that only works when the other defences work is not a defence.
package sponsor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var (
	// ErrWouldAbort is the contract saying no, before anybody signs anything.
	ErrWouldAbort = errors.New("claim_would_fail")
	// ErrSimulationUnavailable is us being unable to ask.
	//
	// Distinct from ErrWouldAbort because they are opposite facts: one is
	// evidence about the claim, the other is evidence about the node. Naming the
	// second after the first would refuse a good claim on the strength of our own
	// outage.
	ErrSimulationUnavailable = errors.New("simulation_unavailable")
)

// Simulation is what the node said would happen.
type Simulation struct {
	Success  bool
	GasUsed  int64
	VMStatus string
}

// Simulator runs Move code against real state without submitting anything.
type Simulator struct {
	nodeURL string
	http    *http.Client
}

func NewSimulator(nodeURL string) *Simulator {
	return &Simulator{nodeURL: nodeURL, http: &http.Client{Timeout: 15 * time.Second}}
}

// ClaimArgs is everything a claim transaction needs, none of it secret.
type ClaimArgs struct {
	Module       string // the address the escrow module is published under
	Escrow       string
	IdentityHash string // 0x-prefixed, 32 bytes
	AmountMinor  string // decimal, as a string: a u64 does not fit a float64
	Proof        []string
	Sender       string // the claimant's address
	SenderPubKey string // their PUBLIC key - no signature is needed
	SeqNumber    string
	FeePayer     string
}

const zeroSig = "0x" + "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

// SimulateClaim asks the chain what a claim would do, BEFORE it is signed.
//
// # Verified against testnet, not assumed
//
// An Aptos fee-payer transaction simulates with a ZERO signature. What the node
// checks is that the public key matches the account's authentication key; it
// does not verify the signature. Measured on the real node: a valid claim
// returns success with gas_used 4431, and the same claim with a wrong amount
// aborts with gas_used 49.
//
// That ordering is the whole point. The contributor never sees a wallet prompt
// for an action we already know fails, and "you have already claimed this" is
// something we can say before asking them to approve anything.
func (s *Simulator) SimulateClaim(ctx context.Context, a ClaimArgs) (Simulation, error) {
	body := map[string]any{
		"sender":                    a.Sender,
		"sequence_number":           a.SeqNumber,
		"max_gas_amount":            "100000",
		"gas_unit_price":            "100",
		"expiration_timestamp_secs": fmt.Sprintf("%d", time.Now().Add(10*time.Minute).Unix()),
		"payload": map[string]any{
			"type":           "entry_function_payload",
			"function":       a.Module + "::escrow::claim",
			"type_arguments": []string{},
			"arguments":      []any{a.Escrow, a.IdentityHash, a.AmountMinor, a.Proof},
		},
		"signature": map[string]any{
			"type": "fee_payer_signature",
			// A zero signature. The node needs the KEY, not a signature, which is
			// what lets this run before the contributor has signed anything.
			"sender": map[string]any{
				"type": "ed25519_signature", "public_key": a.SenderPubKey, "signature": zeroSig,
			},
			"secondary_signer_addresses": []string{},
			"secondary_signers":          []any{},
			"fee_payer_address":          a.FeePayer,
			"fee_payer_signer": map[string]any{
				"type": "ed25519_signature", "public_key": a.SenderPubKey, "signature": zeroSig,
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Simulation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.nodeURL+"/v1/transactions/simulate", bytes.NewReader(raw))
	if err != nil {
		return Simulation{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := s.http.Do(req)
	if err != nil {
		return Simulation{}, fmt.Errorf("%w: %v", ErrSimulationUnavailable, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return Simulation{}, fmt.Errorf("%w: status %d: %s", ErrSimulationUnavailable, res.StatusCode, string(out))
	}

	var results []struct {
		Success  bool   `json:"success"`
		GasUsed  string `json:"gas_used"`
		VMStatus string `json:"vm_status"`
	}
	if err := json.Unmarshal(out, &results); err != nil || len(results) == 0 {
		return Simulation{}, fmt.Errorf("%w: unexpected body: %s", ErrSimulationUnavailable, string(out))
	}
	r := results[0]
	sim := Simulation{Success: r.Success, VMStatus: r.VMStatus}
	fmt.Sscanf(r.GasUsed, "%d", &sim.GasUsed)
	return sim, nil
}
