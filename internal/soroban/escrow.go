package soroban

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// EscrowContract provides methods to interact with the BountyEscrowContract
type EscrowContract struct {
	client          *Client
	txBuilder       *TransactionBuilder
	contractAddress string
}

// NewEscrowContract creates a new escrow contract client
func NewEscrowContract(client *Client, txBuilder *TransactionBuilder, contractAddress string) *EscrowContract {
	return &EscrowContract{
		client:          client,
		txBuilder:       txBuilder,
		contractAddress: contractAddress,
	}
}

// Init initializes the escrow contract with admin and token addresses
func (ec *EscrowContract) Init(ctx context.Context, adminAddress, tokenAddress string) (*TransactionResult, error) {
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

	// Wait for confirmation
	confirmed, err := ec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// LockFunds locks funds for a specific bounty
func (ec *EscrowContract) LockFunds(ctx context.Context, depositorAddress string, bountyID uint64, amount int64, deadline int64) (*TransactionResult, error) {
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
func (ec *EscrowContract) ReleaseFunds(ctx context.Context, bountyID uint64, contributorAddress string) (*TransactionResult, error) {
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
func (ec *EscrowContract) Refund(ctx context.Context, bountyID uint64) (*TransactionResult, error) {
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
func (ec *EscrowContract) GetEscrowInfo(ctx context.Context, bountyID uint64) (*EscrowData, error) {
	// This is a read-only operation, so we use RPC simulation
	return ec.getEscrowInfoRPC(ctx, bountyID)
}

// getEscrowInfoRPC simulates the get_escrow_info contract call and decodes the
// returned ScMap into an EscrowData struct.
//
// Expected contract return layout (ScMap with ScvSymbol keys):
//
//	{ "depositor": ScvAddress, "amount": ScvI64, "status": ScvSymbol, "deadline": ScvI64 }
//
// All fields are validated before use; an error is returned for any unexpected type.
func (ec *EscrowContract) getEscrowInfoRPC(ctx context.Context, bountyID uint64) (*EscrowData, error) {
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	bountyIDVal, err := EncodeScValUint64(bountyID)
	if err != nil {
		return nil, fmt.Errorf("encode bounty_id: %w", err)
	}

	result, err := ec.client.SimulateAndDecode(ctx, contractAddr, "get_escrow_info", []xdr.ScVal{bountyIDVal})
	if err != nil {
		return nil, fmt.Errorf("simulate get_escrow_info: %w", err)
	}

	fields, err := DecodeScValStruct(result)
	if err != nil {
		return nil, fmt.Errorf("decode escrow struct: %w", err)
	}

	depositor, err := DecodeScValAddress(fields["depositor"])
	if err != nil {
		return nil, fmt.Errorf("decode depositor: %w", err)
	}

	amount, err := DecodeScValInt64(fields["amount"])
	if err != nil {
		return nil, fmt.Errorf("decode amount: %w", err)
	}

	statusStr, err := DecodeScValSymbol(fields["status"])
	if err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}

	deadline, err := DecodeScValInt64(fields["deadline"])
	if err != nil {
		return nil, fmt.Errorf("decode deadline: %w", err)
	}

	return &EscrowData{
		Depositor: depositor,
		Amount:    amount,
		Status:    EscrowStatus(statusStr),
		Deadline:  deadline,
	}, nil
}

// GetBalance retrieves the contract balance (read-only)
func (ec *EscrowContract) GetBalance(ctx context.Context) (int64, error) {
	// Similar to GetEscrowInfo, uses RPC simulation
	return ec.getBalanceRPC(ctx)
}

// getBalanceRPC simulates the get_balance contract call and returns the int64 balance.
func (ec *EscrowContract) getBalanceRPC(ctx context.Context) (int64, error) {
	contractAddr, err := EncodeContractAddress(ec.contractAddress)
	if err != nil {
		return 0, fmt.Errorf("invalid contract address: %w", err)
	}

	result, err := ec.client.SimulateAndDecode(ctx, contractAddr, "get_balance", nil)
	if err != nil {
		return 0, fmt.Errorf("simulate get_balance: %w", err)
	}

	balance, err := DecodeScValInt64(result)
	if err != nil {
		return 0, fmt.Errorf("decode balance: %w", err)
	}

	return balance, nil
}
