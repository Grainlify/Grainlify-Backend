package soroban

import (
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go/xdr"
)

// ErrConfirmationUnknown wraps the error returned by a fund-moving contract
// method (EscrowContract.Init/LockFunds/ReleaseFunds/Refund,
// ProgramEscrowContract.LockProgramFunds/SinglePayout/BatchPayout) when the
// submitted transaction's on-chain confirmation could not be verified --
// WaitForConfirmation timed out or errored, not that the transaction failed.
//
// Callers must not treat this as a hard failure to retry from scratch: the
// transaction may still be submitted and could later confirm on-chain, so
// blindly retrying risks a double-submission of a funds-moving operation.
// Use errors.Is(err, ErrConfirmationUnknown) to detect this case, and poll
// for the transaction's actual status later using the TransactionResult
// (still returned alongside this error) and its Hash field.
var ErrConfirmationUnknown = errors.New("transaction submitted but confirmation could not be verified")

// wrapConfirmationUnknown wraps waitErr (a WaitForConfirmation failure) with
// ErrConfirmationUnknown so callers can distinguish "confirmation unknown"
// from "confirmed" via errors.Is, while still returning result -- with its
// tx hash and "pending" status -- so the caller can poll for it later.
func wrapConfirmationUnknown(result *TransactionResult, waitErr error) (*TransactionResult, error) {
	return result, fmt.Errorf("%w: tx_hash=%s: %v", ErrConfirmationUnknown, result.Hash, waitErr)
}

// Network represents the Stellar network (testnet or mainnet)
type Network string

const (
	NetworkTestnet Network = "testnet"
	NetworkMainnet Network = "mainnet"
)

// EscrowStatus represents the status of an escrow
type EscrowStatus string

const (
	EscrowStatusLocked   EscrowStatus = "Locked"
	EscrowStatusReleased EscrowStatus = "Released"
	EscrowStatusRefunded EscrowStatus = "Refunded"
)

// EscrowData represents escrow information from the contract
type EscrowData struct {
	Depositor string       `json:"depositor"`
	Amount    int64        `json:"amount"`
	Status    EscrowStatus `json:"status"`
	Deadline  int64        `json:"deadline"`
}

// ProgramEscrowData represents program escrow information
type ProgramEscrowData struct {
	ProgramID           string `json:"program_id"`
	TotalFunds          int64  `json:"total_funds"`
	RemainingBalance    int64  `json:"remaining_balance"`
	AuthorizedPayoutKey string `json:"authorized_payout_key"`
	TokenAddress        string `json:"token_address"`
}

// TransactionResult represents the result of a transaction submission
type TransactionResult struct {
	Hash      string    `json:"hash"`
	Ledger    uint32    `json:"ledger,omitempty"`
	Status    string    `json:"status"`
	Submitted time.Time `json:"submitted"`
	Confirmed time.Time `json:"confirmed,omitempty"`
}

// ContractAddress represents a Soroban contract address
type ContractAddress struct {
	xdr.ScAddress
}

// String returns the string representation of the contract address
func (ca *ContractAddress) String() string {
	// Convert ScAddress to string representation
	if ca.ContractId != nil {
		return fmt.Sprintf("%x", ca.ContractId[:])
	}
	return ""
}

// RetryConfig configures retry behavior for transactions
type RetryConfig struct {
	MaxRetries      int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	BackoffMultiplier float64
}

// DefaultRetryConfig returns a default retry configuration
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        3,
		InitialDelay:      time.Second,
		MaxDelay:          30 * time.Second,
		BackoffMultiplier: 2.0,
	}
}
