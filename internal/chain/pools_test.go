package chain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const dec = int32(6)

// twoChainEvent registers two mock adapters with different chain ids and
// returns the pools for one event across both. §10 stage 1 requires exactly
// this: a two-chain event run end to end with no real chain involved.
func twoChainEvent(t *testing.T) (*Registry, *MockAdapter, *MockAdapter, []Pool) {
	t.Helper()
	a := NewMockAdapter("soroban", dec)
	b := NewMockAdapter("flare", dec)
	reg := NewRegistry()
	reg.Register(a)
	reg.Register(b)

	const hack = "hack-1"
	pools := []Pool{
		{
			HackathonID: hack, ChainID: "soroban",
			ContributorPool: NewAmount(30_000_000000, dec), MaintainerPool: NewAmount(5_000_000000, dec),
			EscrowRef:        EscrowRef{ChainID: "soroban", HackathonID: hack},
			MinConfirmations: 2,
		},
		{
			HackathonID: hack, ChainID: "flare",
			ContributorPool: NewAmount(30_000_000000, dec), MaintainerPool: NewAmount(5_000_000000, dec),
			EscrowRef:        EscrowRef{ChainID: "flare", HackathonID: hack},
			MinConfirmations: 3,
		},
	}
	return reg, a, b, pools
}

// The full lifecycle across two chains, with no chain-specific branching
// anywhere above the adapter.
func TestTwoChainEvent_FullLifecycleAgainstMocks(t *testing.T) {
	ctx := context.Background()
	reg, soroban, flare, pools := twoChainEvent(t)

	var configHash [32]byte
	copy(configHash[:], "canonical-config-snapshot-hash")

	// --- Phase 3: fund escrow on every chain -------------------------------
	res := FundAllEscrows(ctx, reg, pools, configHash)
	if err := res.Err(); err != nil {
		t.Fatalf("funding failed: %v", err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("dispatched to %d chains, want 2", len(res.Results))
	}

	// Nothing signed anything: every operation produced an unsigned tx.
	for _, r := range res.Results {
		if len(r.Tx.Payload) == 0 || r.Tx.Summary == "" {
			t.Errorf("%s: built tx has no payload or no human-readable summary", r.ChainID)
		}
	}

	// Unconfirmed funding is not funding - the transition must block.
	if _, err := VerifyAllEscrowsFunded(ctx, reg, pools); err == nil {
		t.Fatal("escrow verified as funded with zero confirmations; the live transition would have proceeded on an unfunded promise")
	}

	// Confirm on one chain only. Still blocked: never go live partially funded.
	soroban.ConfirmAll(5)
	readiness, err := VerifyAllEscrowsFunded(ctx, reg, pools)
	if err == nil {
		t.Fatal("escrow verified with only one of two chains confirmed")
	}
	if !strings.Contains(err.Error(), "flare") {
		t.Errorf("the error does not name the failing chain: %v", err)
	}
	for _, r := range readiness {
		if r.ChainID == "soroban" && !r.Ready {
			t.Errorf("soroban should be ready: %s", r.Reason)
		}
	}

	// Both confirmed: now it may go live.
	flare.ConfirmAll(5)
	if _, err := VerifyAllEscrowsFunded(ctx, reg, pools); err != nil {
		t.Fatalf("both chains confirmed but verification still failed: %v", err)
	}

	// --- Config hash to every chain ----------------------------------------
	if err := PublishConfigHashAll(ctx, reg, pools, configHash).Err(); err != nil {
		t.Fatalf("publishing the config hash failed: %v", err)
	}
	for _, p := range pools {
		adapter, _ := reg.For(p.ChainID)
		got, ok, err := adapter.GetCommitment(ctx, p.EscrowRef, CommitmentKey(CommitmentConfigHash, ""))
		if err != nil || !ok {
			t.Fatalf("%s: config hash missing on chain (ok=%v err=%v)", p.ChainID, ok, err)
		}
		if got != configHash {
			t.Errorf("%s: config hash on chain does not match the one published", p.ChainID)
		}
	}

	// --- Draw commit-reveal, on the issue's own chain ----------------------
	const issueRef = "issue-42"
	var commit [32]byte
	copy(commit[:], "H(seed||issue-42)")
	sorobanPool := pools[0]
	adapter, _ := reg.For(sorobanPool.ChainID)

	if _, err := adapter.BuildCommitDrawSeed(ctx, sorobanPool.EscrowRef, issueRef, commit); err != nil {
		t.Fatalf("commit draw seed: %v", err)
	}
	// The commit landed on soroban only - draws are per issue and each issue
	// belongs to exactly one chain.
	if _, ok, _ := flare.GetCommitment(ctx, pools[1].EscrowRef, CommitmentKey(CommitmentDrawCommit, issueRef)); ok {
		t.Error("the draw commit appeared on the other chain; commits go only to the issue's chain")
	}

	// --- Claim root at settle ----------------------------------------------
	var root [32]byte
	copy(root[:], "merkle-root-soroban")
	if _, err := adapter.BuildPublishClaimRoot(ctx, sorobanPool.EscrowRef, root, NewAmount(29_999_000000, dec)); err != nil {
		t.Fatalf("publish claim root: %v", err)
	}
}

// A root may never total more than the chain escrowed. Chain guard and
// backend guard are independent (§5.4) and both must hold.
func TestPublishClaimRoot_RefusesToExceedThatChainsEscrow(t *testing.T) {
	ctx := context.Background()
	reg, soroban, _, pools := twoChainEvent(t)
	if err := FundAllEscrows(ctx, reg, pools, [32]byte{}).Err(); err != nil {
		t.Fatalf("fund: %v", err)
	}
	soroban.ConfirmAll(5)

	var root [32]byte
	over := NewAmount(30_000_000001, dec) // one minor unit above the pool
	_, err := soroban.BuildPublishClaimRoot(ctx, pools[0].EscrowRef, root, over)
	if !errors.Is(err, ErrPoolMismatch) {
		t.Fatalf("err = %v, want ErrPoolMismatch - a root larger than the escrow cannot be honoured", err)
	}
}

// Partial failure on one chain must fail the whole operation and name the
// chain, never succeed quietly on the others.
func TestDispatch_PartialFailureNamesTheChainAndFailsOverall(t *testing.T) {
	ctx := context.Background()
	reg, _, flare, pools := twoChainEvent(t)
	flare.FailBuild["fund_escrow"] = errors.New("rpc unavailable")

	res := FundAllEscrows(ctx, reg, pools, [32]byte{})
	err := res.Err()
	if err == nil {
		t.Fatal("a chain failed to fund but the dispatch reported success")
	}
	if !errors.Is(err, ErrPartialFailure) {
		t.Errorf("err = %v, want ErrPartialFailure", err)
	}
	if !strings.Contains(err.Error(), "flare") || !strings.Contains(err.Error(), "rpc unavailable") {
		t.Errorf("error does not name the chain and cause: %v", err)
	}

	// The healthy chain was still attempted, so an operator sees the whole
	// picture rather than one problem per deploy.
	if len(res.Results) != 2 {
		t.Errorf("dispatch stopped early: %d results, want 2", len(res.Results))
	}
	if got := res.Failed(); len(got) != 1 || got[0] != "flare" {
		t.Errorf("Failed() = %v, want [flare]", got)
	}
}

// §3.4: a confirmed action that later reorgs out must return to submitted.
// Verification re-reads the chain, so it must notice.
func TestVerifyEscrow_ReorgReturnsToUnfunded(t *testing.T) {
	ctx := context.Background()
	reg, soroban, flare, pools := twoChainEvent(t)
	if err := FundAllEscrows(ctx, reg, pools, [32]byte{}).Err(); err != nil {
		t.Fatalf("fund: %v", err)
	}
	soroban.ConfirmAll(5)
	flare.ConfirmAll(5)
	if _, err := VerifyAllEscrowsFunded(ctx, reg, pools); err != nil {
		t.Fatalf("both confirmed: %v", err)
	}

	// The soroban funding transaction reorgs out.
	soroban.Reorg(soroban.FundingTxFor(pools[0].EscrowRef))

	readiness, err := VerifyAllEscrowsFunded(ctx, reg, pools)
	if err == nil {
		t.Fatal("a reorged-out escrow still verified as funded; the backend trusted a remembered confirmation")
	}
	for _, r := range readiness {
		if r.ChainID == "soroban" {
			if r.Ready {
				t.Error("soroban reported ready after its funding reorged out")
			}
			// And the balance reads as zero, not the remembered amount.
			if !r.State.ContributorPool.IsZero() {
				t.Errorf("contributor pool reads %s after a reorg, want zero", r.State.ContributorPool)
			}
		}
	}
}

// §3.3: confirmation depth is per chain. A chain needing 3 must not be
// treated as ready at 2 just because another chain would have been.
func TestVerifyEscrow_ConfirmationDepthIsPerChain(t *testing.T) {
	ctx := context.Background()
	reg, soroban, flare, pools := twoChainEvent(t) // soroban needs 2, flare needs 3
	if err := FundAllEscrows(ctx, reg, pools, [32]byte{}).Err(); err != nil {
		t.Fatalf("fund: %v", err)
	}
	soroban.ConfirmAll(2)
	flare.ConfirmAll(2)

	readiness, err := VerifyAllEscrowsFunded(ctx, reg, pools)
	if err == nil {
		t.Fatal("flare needs 3 confirmations but verified at 2")
	}
	byChain := map[string]EscrowReadiness{}
	for _, r := range readiness {
		byChain[r.ChainID] = r
	}
	if !byChain["soroban"].Ready {
		t.Errorf("soroban needs 2 and had 2: %s", byChain["soroban"].Reason)
	}
	if byChain["flare"].Ready {
		t.Error("flare reported ready below its own confirmation threshold")
	}
}

// §5.3: the draw refuses to run without a confirmed prior commit. Running it
// anyway silently converts a verifiable draw into a trusted one.
func TestGuardDrawCommitConfirmed_RefusesWithoutAConfirmedCommit(t *testing.T) {
	ctx := context.Background()
	reg, soroban, _, pools := twoChainEvent(t)
	p := pools[0]
	const issueRef = "issue-7"

	// No commit submitted at all.
	if err := GuardDrawCommitConfirmed(ctx, reg, p, issueRef, ""); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("err = %v, want ErrNotConfirmed when no commit exists", err)
	}

	// Submitted but unconfirmed.
	var commit [32]byte
	tx, err := soroban.BuildCommitDrawSeed(ctx, p.EscrowRef, issueRef, commit)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	txHash := string(tx.Payload)
	if err := GuardDrawCommitConfirmed(ctx, reg, p, issueRef, txHash); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("err = %v, want ErrNotConfirmed for an unconfirmed commit", err)
	}

	// Confirmed to the chain's own depth: the draw may run.
	soroban.Confirm(txHash, p.MinConfirmations)
	if err := GuardDrawCommitConfirmed(ctx, reg, p, issueRef, txHash); err != nil {
		t.Fatalf("a confirmed commit should permit the draw: %v", err)
	}

	// And if it reorgs out, the guard closes again.
	soroban.Reorg(txHash)
	if err := GuardDrawCommitConfirmed(ctx, reg, p, issueRef, txHash); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("err = %v, want the guard to close after a reorg", err)
	}
}

// An unregistered chain is an error naming the chain, not a silent skip - a
// skipped chain is an event that goes live owing money it never escrowed.
func TestDispatch_UnknownChainIsAFailureNotASkip(t *testing.T) {
	ctx := context.Background()
	reg, _, _, pools := twoChainEvent(t)
	pools = append(pools, Pool{
		HackathonID: "hack-1", ChainID: "starknet",
		ContributorPool: NewAmount(1, dec), MaintainerPool: NewAmount(0, dec),
		EscrowRef: EscrowRef{ChainID: "starknet", HackathonID: "hack-1"},
	})

	err := FundAllEscrows(ctx, reg, pools, [32]byte{}).Err()
	if !errors.Is(err, ErrPartialFailure) {
		t.Fatalf("err = %v, want partial failure for an unregistered chain", err)
	}
	if !strings.Contains(err.Error(), "starknet") {
		t.Errorf("error does not name the unregistered chain: %v", err)
	}
}

// A single-chain event is the same loop with one entry - no special case.
func TestDispatch_SingleChainIsJustTheLoopWithOneEntry(t *testing.T) {
	ctx := context.Background()
	a := NewMockAdapter("soroban", dec)
	reg := NewRegistry()
	reg.Register(a)
	pools := []Pool{{
		HackathonID: "solo", ChainID: "soroban",
		ContributorPool: NewAmount(1_000_000000, dec), MaintainerPool: NewAmount(0, dec),
		EscrowRef:        EscrowRef{ChainID: "soroban", HackathonID: "solo"},
		MinConfirmations: 1,
	}}
	if err := FundAllEscrows(ctx, reg, pools, [32]byte{}).Err(); err != nil {
		t.Fatalf("single-chain fund: %v", err)
	}
	a.ConfirmAll(1)
	if _, err := VerifyAllEscrowsFunded(ctx, reg, pools); err != nil {
		t.Fatalf("single-chain verify: %v", err)
	}
}

// Amounts carry their precision, and comparing across precisions is an error
// rather than a silent coercion - money must never be quietly rescaled.
func TestAmount_PrecisionMismatchIsAnError(t *testing.T) {
	six := NewAmount(1_000000, 6)
	seven := NewAmount(1_0000000, 7)
	if _, err := six.Cmp(seven); err == nil {
		t.Error("comparing a 6-decimal and 7-decimal amount silently succeeded")
	}
	if cmp, err := six.Cmp(NewAmount(2_000000, 6)); err != nil || cmp >= 0 {
		t.Errorf("same-precision compare: cmp=%d err=%v", cmp, err)
	}
}

// The mock must implement the whole interface, or stage 1 cannot prove the
// abstraction.
var _ ChainAdapter = (*MockAdapter)(nil)
