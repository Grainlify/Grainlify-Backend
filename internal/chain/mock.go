package chain

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MockAdapter is a full in-memory ChainAdapter (§10 stage 1).
//
// It exists so every state machine - escrow, commitments, confirmation waits,
// reorg handling, multi-pool dispatch and partial failure - is testable with
// no chain involved. The spec is explicit that if the state machines cannot be
// fully exercised against this, the abstraction is wrong and stage 1 is where
// to find that out.
//
// It is deliberately controllable rather than merely fake: tests can hold a
// transaction below the confirmation threshold, reorg a confirmed one back
// out, or fail a specific operation on one chain only. Those are the paths
// that matter and the ones a happy-path fake would never reach.
type MockAdapter struct {
	mu sync.Mutex

	chainID string
	asset   AssetSpec

	// Confirmations returned per tx hash. Absent means zero.
	confirmations map[string]int
	// AutoConfirm is the depth given to a newly submitted transaction.
	// Zero keeps everything pending until a test advances it, which is the
	// setting that actually exercises the waiting states.
	AutoConfirm int

	escrows     map[string]*mockEscrow
	commitments map[string][32]byte
	claims      map[[32]byte]ClaimState

	// FailBuild forces a build failure for a given Kind, so partial failure
	// across chains can be provoked on exactly one chain.
	FailBuild map[string]error
	// FailVerify forces VerifyEscrowFunded to error.
	FailVerify error

	// AddressPrefix lets a test give this mock a realistic per-chain address
	// shape ("G" for Stellar, "0x" for an EVM chain) so cross-chain
	// rejection can be exercised as it will actually occur. Empty falls back
	// to the generic "<chain-id>:" scheme.
	AddressPrefix string
	// AddressMinLen is the shortest address this chain accepts.
	AddressMinLen int

	// built records every UnsignedTx produced, so tests can assert what
	// would have been signed without anything signing it.
	built []UnsignedTx
	seq   int
}

type mockEscrow struct {
	contributor Amount
	maintainer  Amount
	fundedTx    string
	claimRoot   [32]byte
	rootTotal   Amount
	swept       bool
}

func NewMockAdapter(chainID string, decimals int32) *MockAdapter {
	return &MockAdapter{
		chainID:       chainID,
		asset:         AssetSpec{Symbol: "USDC", Decimals: decimals, Contract: "mock-" + chainID},
		confirmations: map[string]int{},
		escrows:       map[string]*mockEscrow{},
		commitments:   map[string][32]byte{},
		claims:        map[[32]byte]ClaimState{},
		FailBuild:     map[string]error{},
	}
}

func (m *MockAdapter) ChainID() string        { return m.chainID }
func (m *MockAdapter) NativeAsset() AssetSpec { return m.asset }

func (m *MockAdapter) nextTx(kind string) string {
	m.seq++
	return fmt.Sprintf("%s-tx-%s-%d", m.chainID, kind, m.seq)
}

func (m *MockAdapter) build(kind, summary string, ref EscrowRef) (UnsignedTx, string, error) {
	if err, ok := m.FailBuild[kind]; ok && err != nil {
		return UnsignedTx{}, "", err
	}
	txHash := m.nextTx(kind)
	tx := UnsignedTx{
		ChainID: m.chainID,
		Kind:    kind,
		Payload: []byte(txHash),
		Summary: summary,
	}
	m.built = append(m.built, tx)
	m.confirmations[txHash] = m.AutoConfirm
	return tx, txHash, nil
}

// ---------------------------------------------------------------------------
// Escrow
// ---------------------------------------------------------------------------

func (m *MockAdapter) BuildFundEscrow(_ context.Context, p EscrowParams) (UnsignedTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, txHash, err := m.build("fund_escrow",
		fmt.Sprintf("fund %s: contributor=%s maintainer=%s", p.Ref, p.ContributorPool, p.MaintainerPool), p.Ref)
	if err != nil {
		return UnsignedTx{}, err
	}
	// Building does not fund. The balance only appears once the transaction
	// is confirmed, which is what makes the pending state real rather than
	// decorative.
	m.escrows[p.Ref.String()] = &mockEscrow{
		contributor: p.ContributorPool,
		maintainer:  p.MaintainerPool,
		fundedTx:    txHash,
	}
	return tx, nil
}

func (m *MockAdapter) VerifyEscrowFunded(_ context.Context, ref EscrowRef) (FundedState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailVerify != nil {
		return FundedState{}, m.FailVerify
	}
	e, ok := m.escrows[ref.String()]
	if !ok {
		return FundedState{}, nil
	}
	conf := m.confirmations[e.fundedTx]
	st := FundedState{Exists: true, Confirmations: conf}
	// Balances read as zero until the funding transaction has any
	// confirmation - an unconfirmed fund has not happened.
	if conf > 0 {
		st.ContributorPool = e.contributor
		st.MaintainerPool = e.maintainer
	} else {
		st.ContributorPool = Amount{Minor: bigZero(), Decimals: e.contributor.Decimals}
		st.MaintainerPool = Amount{Minor: bigZero(), Decimals: e.maintainer.Decimals}
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Commitments
// ---------------------------------------------------------------------------

func (m *MockAdapter) BuildPublishConfigHash(_ context.Context, ref EscrowRef, hash [32]byte) (UnsignedTx, error) {
	return m.commit(ref, CommitmentConfigHash, "", hash)
}

func (m *MockAdapter) BuildCommitDrawSeed(_ context.Context, ref EscrowRef, issueRef string, c [32]byte) (UnsignedTx, error) {
	return m.commit(ref, CommitmentDrawCommit, issueRef, c)
}

func (m *MockAdapter) BuildRevealDrawSeed(_ context.Context, ref EscrowRef, issueRef string, seed []byte) (UnsignedTx, error) {
	var v [32]byte
	copy(v[:], seed)
	return m.commit(ref, CommitmentDrawReveal, issueRef, v)
}

func (m *MockAdapter) commit(ref EscrowRef, kind, subject string, value [32]byte) (UnsignedTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx, _, err := m.build(kind, fmt.Sprintf("%s %s %s", kind, ref, subject), ref)
	if err != nil {
		return UnsignedTx{}, err
	}
	m.commitments[commitKey(ref, kind, subject)] = value
	return tx, nil
}

func commitKey(ref EscrowRef, kind, subject string) string {
	return strings.Join([]string{ref.String(), kind, subject}, "|")
}

func (m *MockAdapter) GetCommitment(_ context.Context, ref EscrowRef, key string) ([32]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, subject := splitCommitKey(key)
	v, ok := m.commitments[commitKey(ref, kind, subject)]
	return v, ok, nil
}

func splitCommitKey(key string) (kind, subject string) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return key, ""
}

// CommitmentKey builds the key GetCommitment expects, so callers do not
// hand-assemble it and drift apart from the adapter.
func CommitmentKey(kind, subject string) string { return kind + ":" + subject }

// ---------------------------------------------------------------------------
// Claims
// ---------------------------------------------------------------------------

func (m *MockAdapter) BuildPublishClaimRoot(_ context.Context, ref EscrowRef, root [32]byte, total Amount) (UnsignedTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.escrows[ref.String()]
	if !ok {
		return UnsignedTx{}, fmt.Errorf("%w: no escrow for %s", ErrNotFunded, ref)
	}
	// §5.4: a chain's root total must not exceed what that chain escrowed.
	// Enforced here as well as off-chain, because these are independent
	// guards and both must hold.
	if cmp, err := total.Cmp(e.contributor); err != nil {
		return UnsignedTx{}, err
	} else if cmp > 0 {
		return UnsignedTx{}, fmt.Errorf("%w: root total %s exceeds escrowed contributor pool %s on %s",
			ErrPoolMismatch, total, e.contributor, m.chainID)
	}

	tx, _, err := m.build(CommitmentClaimRoot, fmt.Sprintf("publish claim root for %s total=%s", ref, total), ref)
	if err != nil {
		return UnsignedTx{}, err
	}
	e.claimRoot = root
	e.rootTotal = total
	m.commitments[commitKey(ref, CommitmentClaimRoot, "")] = root
	return tx, nil
}

func (m *MockAdapter) BuildSweepUnclaimed(_ context.Context, ref EscrowRef, dest string) (UnsignedTx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validate(dest); err != nil {
		return UnsignedTx{}, err
	}
	tx, _, err := m.build("sweep_unclaimed", fmt.Sprintf("sweep %s to %s", ref, dest), ref)
	if err != nil {
		return UnsignedTx{}, err
	}
	if e, ok := m.escrows[ref.String()]; ok {
		e.swept = true
	}
	return tx, nil
}

func (m *MockAdapter) GetClaimStatus(_ context.Context, _ EscrowRef, leafHash [32]byte) (ClaimState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claims[leafHash], nil
}

func (m *MockAdapter) ConfirmationsFor(_ context.Context, txHash string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.confirmations[txHash], nil
}

// ---------------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------------

func (m *MockAdapter) ValidateAddress(addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validate(addr)
}

func (m *MockAdapter) validate(addr string) error {
	if addr == "" {
		return fmt.Errorf("%w: empty address for %s", ErrInvalidAddress, m.chainID)
	}
	if m.AddressPrefix != "" {
		if !strings.HasPrefix(addr, m.AddressPrefix) {
			return fmt.Errorf("%w: %q is not a %s address (expected prefix %q)",
				ErrInvalidAddress, addr, m.chainID, m.AddressPrefix)
		}
		if m.AddressMinLen > 0 && len(addr) < m.AddressMinLen {
			return fmt.Errorf("%w: %q is too short for %s (want at least %d characters)",
				ErrInvalidAddress, addr, m.chainID, m.AddressMinLen)
		}
		return nil
	}
	if !strings.HasPrefix(addr, m.chainID+":") || len(addr) <= len(m.chainID)+1 {
		return fmt.Errorf("%w: %q is not a %s address", ErrInvalidAddress, addr, m.chainID)
	}
	return nil
}

func (m *MockAdapter) AddressFormat() AddressFormatSpec {
	if m.AddressPrefix != "" {
		return AddressFormatSpec{
			Description: m.chainID + " address beginning " + m.AddressPrefix,
			Example:     m.AddressPrefix + strings.Repeat("X", 8),
			Pattern:     "^" + m.AddressPrefix,
		}
	}
	return AddressFormatSpec{
		Description: "mock address for " + m.chainID,
		Example:     m.chainID + ":ADDRESS",
	}
}

// ---------------------------------------------------------------------------
// Test controls
// ---------------------------------------------------------------------------

// Confirm sets a transaction's confirmation depth.
func (m *MockAdapter) Confirm(txHash string, depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirmations[txHash] = depth
}

// ConfirmAll advances every known transaction to depth, for the common case
// of "the chain caught up".
func (m *MockAdapter) ConfirmAll(depth int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h := range m.confirmations {
		m.confirmations[h] = depth
	}
}

// Reorg drops a transaction back to zero confirmations (§3.4). A confirmed
// action that reorgs out must return to submitted and be retried, so this is
// the control that proves the backend re-checks rather than trusting a
// remembered confirmation.
func (m *MockAdapter) Reorg(txHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirmations[txHash] = 0
}

// FundingTxFor exposes the funding transaction hash for assertions.
func (m *MockAdapter) FundingTxFor(ref EscrowRef) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.escrows[ref.String()]; ok {
		return e.fundedTx
	}
	return ""
}

// Built returns every unsigned transaction produced so far.
func (m *MockAdapter) Built() []UnsignedTx {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsignedTx, len(m.built))
	copy(out, m.built)
	return out
}

// MarkClaimed simulates a contributor claiming their leaf themselves.
func (m *MockAdapter) MarkClaimed(leafHash [32]byte, txHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims[leafHash] = ClaimState{Known: true, Claimed: true, TxHash: txHash}
}
