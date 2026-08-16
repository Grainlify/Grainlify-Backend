// LEGACY - UNUSED - DOES NOT MATCH ANY DEPLOYED CONTRACT.
//
// This file calls a Soroban contract that does not exist in this project. It
// invokes:
//
//	init, lock_funds, release_funds, refund
//
// The contract that does exist - Stellar-Contracts/contracts/grainhack-escrow,
// 586 lines with 19 passing tests - exposes:
//
//	initialise, fund, commit, get_commitment, publish_root, claim,
//	is_claimed, cancel, sweep, balance, get_state, get_root, leaf
//
// Not one name matches. Wiring this up would build, deploy, sign and submit
// perfectly well, and fail at the contract with an unknown-function error -
// after spending a transaction fee, and with nothing in the code to suggest
// why.
//
// It predates the design change from a bounty-per-escrow model (one escrow per
// bounty, funds pushed to a contributor on release) to the Merkle-claim model
// the spec and the contract now implement (one escrow per event, a root
// published once per pool, contributors pulling their own funds with a proof).
//
// Kept rather than deleted: it is the record of a design that was tried, and
// deleting it would lose the reason the current one looks as it does. Renamed
// so it cannot be picked up by accident - which was the real risk, because it
// reads exactly like a working escrow client.
//
// The rest of this package is fine and is what the real adapter will use:
// tx.go, client.go, rpc.go and xdr_helpers.go are ABI-independent.
//
// Do not extend this file. If you need an escrow client, write one against
// internal/chain.ChainAdapter and the contract that exists.

package soroban

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// LegacyUnusedEscrowContract provides methods to interact with the BountyEscrowContract
type LegacyUnusedEscrowContract struct {
	client          *Client
	txBuilder       *TransactionBuilder
	contractAddress string
}

// NewLegacyUnusedEscrowContract creates a new escrow contract client
func NewLegacyUnusedEscrowContract(client *Client, txBuilder *TransactionBuilder, contractAddress string) *LegacyUnusedEscrowContract {
	return &LegacyUnusedEscrowContract{
		client:          client,
		txBuilder:       txBuilder,
		contractAddress: contractAddress,
	}
}

// Init initializes the escrow contract with admin and token addresses
func (ec *LegacyUnusedEscrowContract) Init(ctx context.Context, adminAddress, tokenAddress string) (*TransactionResult, error) {
	ec.client.LogContractInteraction(ec.contractAddress, "init", map[string]interface{}{
		"admin": adminAddress,
		"token": tokenAddress,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	adminVal, err := EncodeScValAddress(adminAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode admin address: %w", err)
	}

	tokenVal, err := EncodeScValAddress(tokenAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode token address: %w", err)
	}

	args := []xdr.ScVal{adminVal, tokenVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "init", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := ec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	return result, nil
}

// LockFunds locks funds for a specific bounty
func (ec *LegacyUnusedEscrowContract) LockFunds(ctx context.Context, depositorAddress string, bountyID uint64, amount int64, deadline int64) (*TransactionResult, error) {
	ec.client.LogContractInteraction(ec.contractAddress, "lock_funds", map[string]interface{}{
		"depositor": depositorAddress,
		"bounty_id": bountyID,
		"amount":    amount,
		"deadline":  deadline,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	depositorVal, err := EncodeScValAddress(depositorAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode depositor address: %w", err)
	}

	bountyIDVal, err := EncodeScValUint64(bountyID)
	if err != nil {
		return nil, fmt.Errorf("failed to encode bounty_id: %w", err)
	}

	amountVal, err := EncodeScValInt64(amount)
	if err != nil {
		return nil, fmt.Errorf("failed to encode amount: %w", err)
	}

	deadlineVal, err := EncodeScValInt64(deadline)
	if err != nil {
		return nil, fmt.Errorf("failed to encode deadline: %w", err)
	}

	args := []xdr.ScVal{depositorVal, bountyIDVal, amountVal, deadlineVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "lock_funds", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := ec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := ec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		// Return the initial result even if confirmation times out
		return result, nil
	}

	return confirmed, nil
}

// ReleaseFunds releases funds to a contributor (admin only)
func (ec *LegacyUnusedEscrowContract) ReleaseFunds(ctx context.Context, bountyID uint64, contributorAddress string) (*TransactionResult, error) {
	ec.client.LogContractInteraction(ec.contractAddress, "release_funds", map[string]interface{}{
		"bounty_id":   bountyID,
		"contributor": contributorAddress,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	bountyIDVal, err := EncodeScValUint64(bountyID)
	if err != nil {
		return nil, fmt.Errorf("failed to encode bounty_id: %w", err)
	}

	contributorVal, err := EncodeScValAddress(contributorAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode contributor address: %w", err)
	}

	args := []xdr.ScVal{bountyIDVal, contributorVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "release_funds", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := ec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := ec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// Refund refunds funds to the original depositor if deadline has passed
func (ec *LegacyUnusedEscrowContract) Refund(ctx context.Context, bountyID uint64) (*TransactionResult, error) {
	ec.client.LogContractInteraction(ec.contractAddress, "refund", map[string]interface{}{
		"bounty_id": bountyID,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	bountyIDVal, err := EncodeScValUint64(bountyID)
	if err != nil {
		return nil, fmt.Errorf("failed to encode bounty_id: %w", err)
	}

	args := []xdr.ScVal{bountyIDVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "refund", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := ec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := ec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// GetEscrowInfo retrieves escrow information (read-only, uses RPC simulation)
func (ec *LegacyUnusedEscrowContract) GetEscrowInfo(ctx context.Context, bountyID uint64) (*EscrowData, error) {
	// This is a read-only operation, so we use RPC simulation
	return ec.getEscrowInfoRPC(ctx, bountyID)
}

// getEscrowInfoRPC uses Soroban RPC to simulate the get_escrow_info call
func (ec *LegacyUnusedEscrowContract) getEscrowInfoRPC(ctx context.Context, bountyID uint64) (*EscrowData, error) {
	// Build a read-only transaction for simulation
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	bountyIDVal, err := EncodeScValUint64(bountyID)
	if err != nil {
		return nil, fmt.Errorf("failed to encode bounty_id: %w", err)
	}

	args := []xdr.ScVal{bountyIDVal}

	// Build operation
	_, err = BuildInvokeHostFunctionOp(contractAddr, "get_escrow_info", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build transaction (read-only, won't be submitted)
	// For now, we'll use RPC simulation
	// This requires building the transaction XDR and calling simulateTransaction

	// Note: Full implementation requires:
	// 1. Building transaction XDR
	// 2. Calling simulateTransaction via RPC
	// 3. Decoding the ScVal return value
	// 4. Converting to EscrowData struct

	slog.Warn("GetEscrowInfo requires transaction building and XDR decoding")
	return nil, fmt.Errorf("GetEscrowInfo requires transaction building - use RPC simulateTransaction")
}

// GetBalance retrieves the contract balance (read-only)
func (ec *LegacyUnusedEscrowContract) GetBalance(ctx context.Context) (int64, error) {
	// Similar to GetEscrowInfo, uses RPC simulation
	return ec.getBalanceRPC(ctx)
}

// getBalanceRPC uses Soroban RPC to get contract balance
func (ec *LegacyUnusedEscrowContract) getBalanceRPC(ctx context.Context) (int64, error) {
	// Similar to getEscrowInfoRPC - requires transaction building and XDR decoding
	slog.Warn("GetBalance requires transaction building and XDR decoding")
	return 0, fmt.Errorf("GetBalance requires transaction building - use RPC simulateTransaction")
}
