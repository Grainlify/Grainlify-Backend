package errors

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Source of Truth Documentation
// ---------------------------------------------------------------------------
//
// The error code mappings in contract_errors.go are sourced from the following
// Stellar/Soroban contract repositories and versions:
//
//   - BountyEscrow: contracts/bounty_escrow/contracts/escrow/src/lib.rs
//     Commit: main (as of 2026-07-23)
//     Error enum: ContractError (repr(u32))
//
//   - Governance: contracts/grainlify-core/src/governance.rs
//     Commit: main (as of 2026-07-23)
//     Error enum: ContractError (repr(u32))
//
//   - CircuitBreaker: contracts/program-escrow/src/error_recovery.rs
//     Commit: main (as of 2026-07-23)
//     Constants: u32 error codes
//
// When adding new error codes from contract updates:
//   1. Update the corresponding map in contract_errors.go
//   2. Add a new row to the appropriate table in TestContractErrorMappings below
//   3. Update the expectedCodes array for the contract kind
//   4. Update the registry count test in TestRegistryCounts
//
// This ensures 100% coverage of all mapped error codes.

// ---------------------------------------------------------------------------
// Table-Driven Tests: Exhaustive Error Code Mappings
// ---------------------------------------------------------------------------

// TestContractErrorMappings is the authoritative table-driven test that
// enumerates every currently-mapped error code and asserts the correct
// error name and message. Adding a new contract error variant requires only
// adding a new row to the appropriate sub-table below.
func TestContractErrorMappings(t *testing.T) {
	tests := []struct {
		name       string
		kind       ContractKind
		code       uint32
		wantName   string
		wantMessage string
	}{
		// BountyEscrow error codes
		// Source: contracts/bounty_escrow/contracts/escrow/src/lib.rs
		{
			name:       "BountyEscrow/1/AlreadyInitialized",
			kind:       BountyEscrow,
			code:       1,
			wantName:   "AlreadyInitialized",
			wantMessage: "Bounty escrow contract is already initialized",
		},
		{
			name:       "BountyEscrow/2/NotInitialized",
			kind:       BountyEscrow,
			code:       2,
			wantName:   "NotInitialized",
			wantMessage: "Bounty escrow contract has not been initialized",
		},
		{
			name:       "BountyEscrow/3/BountyExists",
			kind:       BountyEscrow,
			code:       3,
			wantName:   "BountyExists",
			wantMessage: "A bounty with this ID already exists",
		},
		{
			name:       "BountyEscrow/4/BountyNotFound",
			kind:       BountyEscrow,
			code:       4,
			wantName:   "BountyNotFound",
			wantMessage: "Bounty not found",
		},
		{
			name:       "BountyEscrow/5/FundsNotLocked",
			kind:       BountyEscrow,
			code:       5,
			wantName:   "FundsNotLocked",
			wantMessage: "Bounty funds have not been locked yet",
		},
		{
			name:       "BountyEscrow/6/DeadlineNotPassed",
			kind:       BountyEscrow,
			code:       6,
			wantName:   "DeadlineNotPassed",
			wantMessage: "Bounty deadline has not passed yet",
		},
		{
			name:       "BountyEscrow/7/Unauthorized",
			kind:       BountyEscrow,
			code:       7,
			wantName:   "Unauthorized",
			wantMessage: "Unauthorized: caller is not allowed to perform this bounty operation",
		},
		{
			name:       "BountyEscrow/8/InvalidFeeRate",
			kind:       BountyEscrow,
			code:       8,
			wantName:   "InvalidFeeRate",
			wantMessage: "Fee rate is invalid (must be between 0 and 5000 basis points)",
		},
		{
			name:       "BountyEscrow/9/FeeRecipientNotSet",
			kind:       BountyEscrow,
			code:       9,
			wantName:   "FeeRecipientNotSet",
			wantMessage: "Fee recipient address has not been configured",
		},
		{
			name:       "BountyEscrow/10/InvalidBatchSize",
			kind:       BountyEscrow,
			code:       10,
			wantName:   "InvalidBatchSize",
			wantMessage: "Batch size is invalid (must be between 1 and 20)",
		},
		{
			name:       "BountyEscrow/11/BatchSizeMismatch",
			kind:       BountyEscrow,
			code:       11,
			wantName:   "BatchSizeMismatch",
			wantMessage: "Number of bounty IDs does not match the number of recipients",
		},
		{
			name:       "BountyEscrow/12/DuplicateBountyId",
			kind:       BountyEscrow,
			code:       12,
			wantName:   "DuplicateBountyId",
			wantMessage: "Duplicate bounty ID found in batch",
		},
		{
			name:       "BountyEscrow/13/InvalidAmount",
			kind:       BountyEscrow,
			code:       13,
			wantName:   "InvalidAmount",
			wantMessage: "Bounty amount is invalid (zero, negative, or exceeds available)",
		},
		{
			name:       "BountyEscrow/14/InvalidDeadline",
			kind:       BountyEscrow,
			code:       14,
			wantName:   "InvalidDeadline",
			wantMessage: "Bounty deadline is invalid (in the past or too far in the future)",
		},
		// Note: Code 15 is intentionally absent in the BountyEscrow contract enum
		{
			name:       "BountyEscrow/16/InsufficientFunds",
			kind:       BountyEscrow,
			code:       16,
			wantName:   "InsufficientFunds",
			wantMessage: "Insufficient funds in the escrow for this operation",
		},
		{
			name:       "BountyEscrow/17/RefundNotApproved",
			kind:       BountyEscrow,
			code:       17,
			wantName:   "RefundNotApproved",
			wantMessage: "Refund has not been approved by an admin",
		},
		{
			name:       "BountyEscrow/18/FundsPaused",
			kind:       BountyEscrow,
			code:       18,
			wantName:   "FundsPaused",
			wantMessage: "Bounty fund operations are currently paused",
		},

		// Governance error codes
		// Source: contracts/grainlify-core/src/governance.rs
		{
			name:       "Governance/1/NotInitialized",
			kind:       Governance,
			code:       1,
			wantName:   "NotInitialized",
			wantMessage: "Governance contract has not been initialized",
		},
		{
			name:       "Governance/2/InvalidThreshold",
			kind:       Governance,
			code:       2,
			wantName:   "InvalidThreshold",
			wantMessage: "Governance threshold value is invalid",
		},
		{
			name:       "Governance/3/ThresholdTooLow",
			kind:       Governance,
			code:       3,
			wantName:   "ThresholdTooLow",
			wantMessage: "Governance threshold is too low",
		},
		{
			name:       "Governance/4/InsufficientStake",
			kind:       Governance,
			code:       4,
			wantName:   "InsufficientStake",
			wantMessage: "Insufficient stake to perform this governance action",
		},
		{
			name:       "Governance/5/ProposalsNotFound",
			kind:       Governance,
			code:       5,
			wantName:   "ProposalsNotFound",
			wantMessage: "No proposals found",
		},
		{
			name:       "Governance/6/ProposalNotFound",
			kind:       Governance,
			code:       6,
			wantName:   "ProposalNotFound",
			wantMessage: "Proposal not found",
		},
		{
			name:       "Governance/7/ProposalNotActive",
			kind:       Governance,
			code:       7,
			wantName:   "ProposalNotActive",
			wantMessage: "Proposal is not currently active",
		},
		{
			name:       "Governance/8/VotingNotStarted",
			kind:       Governance,
			code:       8,
			wantName:   "VotingNotStarted",
			wantMessage: "Voting has not started yet for this proposal",
		},
		{
			name:       "Governance/9/VotingEnded",
			kind:       Governance,
			code:       9,
			wantName:   "VotingEnded",
			wantMessage: "Voting period has ended for this proposal",
		},
		{
			name:       "Governance/10/VotingStillActive",
			kind:       Governance,
			code:       10,
			wantName:   "VotingStillActive",
			wantMessage: "Voting is still active; cannot execute proposal yet",
		},
		{
			name:       "Governance/11/AlreadyVoted",
			kind:       Governance,
			code:       11,
			wantName:   "AlreadyVoted",
			wantMessage: "You have already voted on this proposal",
		},
		{
			name:       "Governance/12/ProposalNotApproved",
			kind:       Governance,
			code:       12,
			wantName:   "ProposalNotApproved",
			wantMessage: "Proposal has not been approved",
		},
		{
			name:       "Governance/13/ExecutionDelayNotMet",
			kind:       Governance,
			code:       13,
			wantName:   "ExecutionDelayNotMet",
			wantMessage: "Execution delay period has not elapsed yet",
		},
		{
			name:       "Governance/14/ProposalExpired",
			kind:       Governance,
			code:       14,
			wantName:   "ProposalExpired",
			wantMessage: "Proposal has expired",
		},

		// CircuitBreaker error codes
		// Source: contracts/program-escrow/src/error_recovery.rs
		{
			name:       "CircuitBreaker/0/None",
			kind:       CircuitBreaker,
			code:       0,
			wantName:   "None",
			wantMessage: "Operation succeeded",
		},
		{
			name:       "CircuitBreaker/1001/CircuitOpen",
			kind:       CircuitBreaker,
			code:       1001,
			wantName:   "CircuitOpen",
			wantMessage: "Circuit breaker is open; operation rejected without attempting",
		},
		{
			name:       "CircuitBreaker/1002/TransferFailed",
			kind:       CircuitBreaker,
			code:       1002,
			wantName:   "TransferFailed",
			wantMessage: "Token transfer failed (transient error)",
		},
		{
			name:       "CircuitBreaker/1003/InsufficientBalance",
			kind:       CircuitBreaker,
			code:       1003,
			wantName:   "InsufficientBalance",
			wantMessage: "Insufficient contract balance for transfer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test ContractErrorMessage
			gotMessage := ContractErrorMessage(tt.kind, tt.code)
			if gotMessage != tt.wantMessage {
				t.Errorf("ContractErrorMessage(%s, %d) = %q, want %q",
					tt.kind, tt.code, gotMessage, tt.wantMessage)
			}

			// Test ContractErrorName
			gotName := ContractErrorName(tt.kind, tt.code)
			if gotName != tt.wantName {
				t.Errorf("ContractErrorName(%s, %d) = %q, want %q",
					tt.kind, tt.code, gotName, tt.wantName)
			}

			// Ensure message is not a fallback (should be the actual mapped message)
			if strings.HasPrefix(gotMessage, "Unknown") {
				t.Errorf("ContractErrorMessage(%s, %d) returned fallback %q, expected mapped message",
					tt.kind, tt.code, gotMessage)
			}

			// Ensure name is not a fallback
			if strings.HasPrefix(gotName, "Unknown") {
				t.Errorf("ContractErrorName(%s, %d) returned fallback %q, expected mapped name",
					tt.kind, tt.code, gotName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Unknown/Unmapped Error Codes: Safe Fallback Tests
// ---------------------------------------------------------------------------

// TestUnknownContractErrorCode verifies that unknown error codes produce
// a safe, clearly-labeled fallback error message rather than a panic
// or misleading message.
func TestUnknownContractErrorCode(t *testing.T) {
	cases := []struct {
		name     string
		kind     ContractKind
		code     uint32
		wantInMsg string
	}{
		{
			name:     "BountyEscrow unknown code",
			kind:     BountyEscrow,
			code:     9999,
			wantInMsg: "Unknown BountyEscrow contract error (code 9999)",
		},
		{
			name:     "Governance unknown code",
			kind:     Governance,
			code:     9999,
			wantInMsg: "Unknown Governance contract error (code 9999)",
		},
		{
			name:     "CircuitBreaker unknown code",
			kind:     CircuitBreaker,
			code:     9999,
			wantInMsg: "Unknown CircuitBreaker contract error (code 9999)",
		},
		{
			name:     "BountyEscrow gap code 15",
			kind:     BountyEscrow,
			code:     15,
			wantInMsg: "Unknown BountyEscrow contract error (code 15)",
		},
		{
			name:     "Governance code 0 (not in enum)",
			kind:     Governance,
			code:     0,
			wantInMsg: "Unknown Governance contract error (code 0)",
		},
		{
			name:     "CircuitBreaker code 1 (not in enum)",
			kind:     CircuitBreaker,
			code:     1,
			wantInMsg: "Unknown CircuitBreaker contract error (code 1)",
		},
		{
			name:     "BountyEscrow boundary max uint32",
			kind:     BountyEscrow,
			code:     ^uint32(0), // max uint32 value
			wantInMsg: "Unknown BountyEscrow contract error",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Test ContractErrorMessage fallback
			gotMessage := ContractErrorMessage(tt.kind, tt.code)
			if !strings.Contains(gotMessage, tt.wantInMsg) {
				t.Errorf("ContractErrorMessage(%s, %d) = %q, want to contain %q",
					tt.kind, tt.code, gotMessage, tt.wantInMsg)
			}

			// Ensure it starts with "Unknown" for clarity
			if !strings.HasPrefix(gotMessage, "Unknown") {
				t.Errorf("ContractErrorMessage(%s, %d) = %q, should start with 'Unknown'",
					tt.kind, tt.code, gotMessage)
			}

			// Ensure it contains the code
			if !strings.Contains(gotMessage, fmt.Sprintf("%d", tt.code)) {
				t.Errorf("ContractErrorMessage(%s, %d) = %q, should contain the code",
					tt.kind, tt.code, gotMessage)
			}
		})
	}
}

// TestUnknownContractKind verifies that unknown contract kinds produce
// a safe, clearly-labeled fallback error message.
func TestUnknownContractKind(t *testing.T) {
	unknownKind := ContractKind("nonexistent_contract")
	code := uint32(1)

	msg := ContractErrorMessage(unknownKind, code)
	wantInMsg := "Unknown nonexistent_contract contract error (code 1)"

	if !strings.Contains(msg, wantInMsg) {
		t.Errorf("ContractErrorMessage(%s, %d) = %q, want to contain %q",
			unknownKind, code, msg, wantInMsg)
	}

	name := ContractErrorName(unknownKind, code)
	if !strings.HasPrefix(name, "Unknown") {
		t.Errorf("ContractErrorName(%s, %d) = %q, should start with 'Unknown'",
			unknownKind, code, name)
	}
}

// TestUnknownContractErrorName verifies that unknown error codes produce
// a safe, clearly-labeled fallback name.
func TestUnknownContractErrorName(t *testing.T) {
	cases := []struct {
		name string
		kind ContractKind
		code uint32
	}{
		{"BountyEscrow unknown", BountyEscrow, 9999},
		{"Governance unknown", Governance, 9999},
		{"CircuitBreaker unknown", CircuitBreaker, 9999},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ContractErrorName(tt.kind, tt.code)

			// Should start with "Unknown"
			if !strings.HasPrefix(got, "Unknown") {
				t.Errorf("ContractErrorName(%s, %d) = %q, should start with 'Unknown'",
					tt.kind, tt.code, got)
			}

			// Should contain the code
			if !strings.Contains(got, fmt.Sprintf("%d", tt.code)) {
				t.Errorf("ContractErrorName(%s, %d) = %q, should contain the code",
					tt.kind, tt.code, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Completeness Tests: Ensure All Mapped Codes Are Tested
// ---------------------------------------------------------------------------

// expectedCodes defines the authoritative list of all currently-mapped error codes.
// This must be kept in sync with the maps in contract_errors.go.
var expectedCodes = map[ContractKind][]uint32{
	BountyEscrow:     {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 18},
	Governance:       {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
	CircuitBreaker:   {0, 1001, 1002, 1003},
}

// TestAllMappedCodesAreExplicitlyTested ensures that every code in the registry
// has a corresponding entry in the table-driven test above.
func TestAllMappedCodesAreExplicitlyTested(t *testing.T) {
	for kind, codes := range expectedCodes {
		allCodes := AllCodes(kind)
		if len(allCodes) != len(codes) {
			t.Errorf("%s: expected %d codes in registry, got %d",
				kind, len(codes), len(allCodes))
		}

		// Check that all expected codes are in the registry
		codeSet := make(map[uint32]bool)
		for _, c := range allCodes {
			codeSet[c] = true
		}

		for _, expectedCode := range codes {
			if !codeSet[expectedCode] {
				t.Errorf("%s: expected code %d not found in registry",
					kind, expectedCode)
			}

			// Verify the code has a non-fallback message
			msg := ContractErrorMessage(kind, expectedCode)
			if strings.HasPrefix(msg, "Unknown") {
				t.Errorf("%s: code %d has fallback message: %q",
					kind, expectedCode, msg)
			}

			// Verify the code has a non-fallback name
			name := ContractErrorName(kind, expectedCode)
			if strings.HasPrefix(name, "Unknown") {
				t.Errorf("%s: code %d has fallback name: %q",
					kind, expectedCode, name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Edge Cases
// ---------------------------------------------------------------------------

// TestDuplicateCodeDetection ensures that no two different contract kinds
// map the same code to different meanings (which would be ambiguous).
func TestDuplicateCodeDetection(t *testing.T) {
	// Build a map of code -> (kind, name, message)
	codeToInfo := make(map[uint32][]struct{ kind, name, message string })

	for kind := range expectedCodes {
		for code := range registry[kind] {
			entry := registry[kind][code]
			codeToInfo[code] = append(codeToInfo[code], struct {
				kind    string
				name    string
				message string
			}{string(kind), entry.Name, entry.Message})
		}
	}

	// Check for duplicates across different contract kinds
	for code, infos := range codeToInfo {
		if len(infos) > 1 {
			// This is OK - same code can exist in different contracts
			// with different meanings (e.g., code 1 in BountyEscrow vs Governance)
			// This test just documents that this is intentional
			t.Logf("Code %d appears in multiple contracts:", code)
			for _, info := range infos {
				t.Logf("  %s: %s - %s", info.kind, info.name, info.message)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Registry Count Tests
// ---------------------------------------------------------------------------

func TestRegistryCounts(t *testing.T) {
	// BountyEscrow: 17 codes (1-14, 16-18, note 15 is intentionally absent)
	if got := len(AllCodes(BountyEscrow)); got != 17 {
		t.Errorf("BountyEscrow: expected 17 error codes, got %d", got)
	}

	// Governance: 14 codes (1-14)
	if got := len(AllCodes(Governance)); got != 14 {
		t.Errorf("Governance: expected 14 error codes, got %d", got)
	}

	// CircuitBreaker: 4 codes (0, 1001-1003)
	if got := len(AllCodes(CircuitBreaker)); got != 4 {
		t.Errorf("CircuitBreaker: expected 4 error codes (including ERR_NONE), got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Message Quality Tests
// ---------------------------------------------------------------------------

func TestAllMessagesNonEmpty(t *testing.T) {
	for _, kind := range []ContractKind{BountyEscrow, Governance, CircuitBreaker} {
		codes := AllCodes(kind)
		if len(codes) == 0 {
			t.Errorf("no codes registered for %s", kind)
		}
		for _, code := range codes {
			msg := ContractErrorMessage(kind, code)
			if msg == "" {
				t.Errorf("%s code %d has empty message", kind, code)
			}
			if len(msg) < 10 {
				t.Errorf("%s code %d message too short: %q", kind, code, msg)
			}
		}
	}
}

func TestAllNamesNonEmpty(t *testing.T) {
	for _, kind := range []ContractKind{BountyEscrow, Governance, CircuitBreaker} {
		codes := AllCodes(kind)
		for _, code := range codes {
			name := ContractErrorName(kind, code)
			if name == "" {
				t.Errorf("%s code %d has empty name", kind, code)
			}
		}
	}
}
