package soroban

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// ProgramEscrowContract provides methods to interact with the ProgramEscrowContract
type ProgramEscrowContract struct {
	client          *Client
	txBuilder       *TransactionBuilder
	contractAddress string
}

// NewProgramEscrowContract creates a new program escrow contract client
func NewProgramEscrowContract(client *Client, txBuilder *TransactionBuilder, contractAddress string) *ProgramEscrowContract {
	return &ProgramEscrowContract{
		client:          client,
		txBuilder:       txBuilder,
		contractAddress: contractAddress,
	}
}

// InitProgram initializes a new program escrow
func (pec *ProgramEscrowContract) InitProgram(ctx context.Context, programID, authorizedPayoutKey, tokenAddress string) (*TransactionResult, error) {
	pec.client.LogContractInteraction(pec.contractAddress, "init_program", map[string]interface{}{
		"program_id":            programID,
		"authorized_payout_key": authorizedPayoutKey,
		"token_address":         tokenAddress,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	programIDVal, err := EncodeScValString(programID)
	if err != nil {
		return nil, fmt.Errorf("failed to encode program_id: %w", err)
	}

	authKeyVal, err := EncodeScValAddress(authorizedPayoutKey)
	if err != nil {
		return nil, fmt.Errorf("failed to encode authorized_payout_key: %w", err)
	}

	tokenVal, err := EncodeScValAddress(tokenAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode token_address: %w", err)
	}

	args := []xdr.ScVal{programIDVal, authKeyVal, tokenVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "init_program", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := pec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	return result, nil
}

// LockProgramFunds locks funds into the program escrow
func (pec *ProgramEscrowContract) LockProgramFunds(ctx context.Context, amount int64) (*TransactionResult, error) {
	pec.client.LogContractInteraction(pec.contractAddress, "lock_program_funds", map[string]interface{}{
		"amount": amount,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	amountVal, err := EncodeScValInt64(amount)
	if err != nil {
		return nil, fmt.Errorf("failed to encode amount: %w", err)
	}

	args := []xdr.ScVal{amountVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "lock_program_funds", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := pec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := pec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// SinglePayout executes a single payout to one recipient
func (pec *ProgramEscrowContract) SinglePayout(ctx context.Context, recipientAddress string, amount int64) (*TransactionResult, error) {
	pec.client.LogContractInteraction(pec.contractAddress, "single_payout", map[string]interface{}{
		"recipient": recipientAddress,
		"amount":    amount,
	})

	// Encode contract address
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode function arguments
	recipientVal, err := EncodeScValAddress(recipientAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to encode recipient address: %w", err)
	}

	amountVal, err := EncodeScValInt64(amount)
	if err != nil {
		return nil, fmt.Errorf("failed to encode amount: %w", err)
	}

	args := []xdr.ScVal{recipientVal, amountVal}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "single_payout", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := pec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := pec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// BatchPayout executes payouts to multiple recipients
type PayoutItem struct {
	Recipient string
	Amount    int64
}

func (pec *ProgramEscrowContract) BatchPayout(ctx context.Context, payouts []PayoutItem) (*TransactionResult, error) {
	pec.client.LogContractInteraction(pec.contractAddress, "batch_payout", map[string]interface{}{
		"payout_count": len(payouts),
	})

	if len(payouts) == 0 {
		return nil, fmt.Errorf("payouts list cannot be empty")
	}

	// Encode contract address
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	// Encode recipients vector
	recipientVals := make([]xdr.ScVal, len(payouts))
	for i, payout := range payouts {
		recipientVal, err := EncodeScValAddress(payout.Recipient)
		if err != nil {
			return nil, fmt.Errorf("failed to encode recipient %d: %w", i, err)
		}
		recipientVals[i] = recipientVal
	}
	recipientsVec, err := EncodeScValVec(recipientVals)
	if err != nil {
		return nil, fmt.Errorf("failed to encode recipients vector: %w", err)
	}

	// Encode amounts vector
	amountVals := make([]xdr.ScVal, len(payouts))
	for i, payout := range payouts {
		amountVal, err := EncodeScValInt64(payout.Amount)
		if err != nil {
			return nil, fmt.Errorf("failed to encode amount %d: %w", i, err)
		}
		amountVals[i] = amountVal
	}
	amountsVec, err := EncodeScValVec(amountVals)
	if err != nil {
		return nil, fmt.Errorf("failed to encode amounts vector: %w", err)
	}

	args := []xdr.ScVal{recipientsVec, amountsVec}

	// Build InvokeHostFunction operation
	op, err := BuildInvokeHostFunctionOp(contractAddr, "batch_payout", args)
	if err != nil {
		return nil, fmt.Errorf("failed to build operation: %w", err)
	}

	// Build and submit transaction
	result, err := pec.txBuilder.BuildAndSubmit(ctx, []txnbuild.Operation{op})
	if err != nil {
		return nil, fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Wait for confirmation
	confirmed, err := pec.txBuilder.WaitForConfirmation(ctx, result.Hash, 60*time.Second)
	if err != nil {
		slog.Warn("failed to wait for confirmation", "error", err, "tx_hash", result.Hash)
		return result, nil
	}

	return confirmed, nil
}

// GetProgramInfo retrieves program information (read-only)
func (pec *ProgramEscrowContract) GetProgramInfo(ctx context.Context) (*ProgramEscrowData, error) {
	return pec.getProgramInfoRPC(ctx)
}

// getProgramInfoRPC simulates the get_program_info contract call and decodes the
// returned ScMap into a ProgramEscrowData struct.
//
// Expected contract return layout (ScMap with ScvSymbol keys):
//
//	{
//	    "program_id":            ScvString,
//	    "total_funds":           ScvI64,
//	    "remaining_balance":     ScvI64,
//	    "authorized_payout_key": ScvAddress,
//	    "token_address":         ScvAddress,
//	}
//
// All fields are validated before use; an error is returned for any unexpected type.
func (pec *ProgramEscrowContract) getProgramInfoRPC(ctx context.Context) (*ProgramEscrowData, error) {
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address: %w", err)
	}

	result, err := pec.client.SimulateAndDecode(ctx, contractAddr, "get_program_info", nil)
	if err != nil {
		return nil, fmt.Errorf("simulate get_program_info: %w", err)
	}

	fields, err := DecodeScValStruct(result)
	if err != nil {
		return nil, fmt.Errorf("decode program struct: %w", err)
	}

	programID, err := DecodeScValString(fields["program_id"])
	if err != nil {
		return nil, fmt.Errorf("decode program_id: %w", err)
	}

	totalFunds, err := DecodeScValInt64(fields["total_funds"])
	if err != nil {
		return nil, fmt.Errorf("decode total_funds: %w", err)
	}

	remainingBalance, err := DecodeScValInt64(fields["remaining_balance"])
	if err != nil {
		return nil, fmt.Errorf("decode remaining_balance: %w", err)
	}

	authKey, err := DecodeScValAddress(fields["authorized_payout_key"])
	if err != nil {
		return nil, fmt.Errorf("decode authorized_payout_key: %w", err)
	}

	tokenAddr, err := DecodeScValAddress(fields["token_address"])
	if err != nil {
		return nil, fmt.Errorf("decode token_address: %w", err)
	}

	return &ProgramEscrowData{
		ProgramID:           programID,
		TotalFunds:          totalFunds,
		RemainingBalance:    remainingBalance,
		AuthorizedPayoutKey: authKey,
		TokenAddress:        tokenAddr,
	}, nil
}

// GetRemainingBalance retrieves the remaining balance (read-only)
func (pec *ProgramEscrowContract) GetRemainingBalance(ctx context.Context) (int64, error) {
	return pec.getRemainingBalanceRPC(ctx)
}

// getRemainingBalanceRPC simulates the get_remaining_balance contract call and
// returns the int64 balance.
func (pec *ProgramEscrowContract) getRemainingBalanceRPC(ctx context.Context) (int64, error) {
	contractAddr, err := EncodeContractAddress(pec.contractAddress)
	if err != nil {
		return 0, fmt.Errorf("invalid contract address: %w", err)
	}

	result, err := pec.client.SimulateAndDecode(ctx, contractAddr, "get_remaining_balance", nil)
	if err != nil {
		return 0, fmt.Errorf("simulate get_remaining_balance: %w", err)
	}

	balance, err := DecodeScValInt64(result)
	if err != nil {
		return 0, fmt.Errorf("decode balance: %w", err)
	}

	return balance, nil
}
