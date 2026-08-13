package chain

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

// leafVector is the cross-implementation pin shared with the Soroban contract.
// The identical vector is asserted from the contract side, so whichever
// implementation moves, one of the two test suites goes red.
type leafVector struct {
	Inputs struct {
		IdentityHashHex string `json:"identity_hash_hex"`
		ClaimAddress    string `json:"claim_address"`
		AmountMinor     int64  `json:"amount_minor"`
	} `json:"inputs"`
	LeafContributor string `json:"leaf_contributor"`
	LeafMaintainer  string `json:"leaf_maintainer"`
}

func loadLeafVector(t *testing.T) leafVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/leaf_vector.json")
	if err != nil {
		t.Fatalf("read leaf vector: %v", err)
	}
	var v leafVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse leaf vector: %v", err)
	}
	return v
}

func (v leafVector) leaf(t *testing.T, pool PoolKind) ClaimLeaf {
	t.Helper()
	idBytes, err := hex.DecodeString(v.Inputs.IdentityHashHex)
	if err != nil || len(idBytes) != 32 {
		t.Fatalf("bad identity hash in vector: %v", err)
	}
	var id [32]byte
	copy(id[:], idBytes)
	return ClaimLeaf{
		Pool:         pool,
		IdentityHash: id,
		ClaimAddress: v.Inputs.ClaimAddress,
		AmountMinor:  big.NewInt(v.Inputs.AmountMinor),
	}
}

// TestClaimLeafHash_MatchesTheContract is the cross-implementation check the
// contract's exported leaf() was always meant to enable.
//
// The two implementations drifted before this existed - the contract hashed
// pool, XDR address, identity and a 16-byte amount, while this hashed identity,
// UTF-8 address, an 8-byte amount, chain id and event id. They agreed on
// nothing, so a root built here could not be claimed against the contract.
// Both now compute the canonical construction of spec §13.1 and are asserted
// against the same digests.
//
// If this fails, one side changed. Fix the side that moved; do not update the
// vector to make it pass.
func TestClaimLeafHash_MatchesTheContract(t *testing.T) {
	v := loadLeafVector(t)

	for _, tc := range []struct {
		pool PoolKind
		want string
	}{
		{PoolKindContributor, v.LeafContributor},
		{PoolKindMaintainer, v.LeafMaintainer},
	} {
		got := v.leaf(t, tc.pool).Hash()
		if hex.EncodeToString(got[:]) != tc.want {
			t.Errorf("%s leaf digest\n got  %s\n want %s", tc.pool, hex.EncodeToString(got[:]), tc.want)
		}
	}
}

// TestClaimLeafHash_PoolIsBoundIntoTheLeaf is §5.1 enforced in the digest: the
// same entitlement in the other pool is a different leaf, so a contributor
// proof cannot verify against the maintainer root.
//
// This builder had no pool binding at all before alignment - the property
// existed in the contract and was absent here.
func TestClaimLeafHash_PoolIsBoundIntoTheLeaf(t *testing.T) {
	v := loadLeafVector(t)
	if v.leaf(t, PoolKindContributor).Hash() == v.leaf(t, PoolKindMaintainer).Hash() {
		t.Fatal("contributor and maintainer leaves collide; the pool is not in the digest")
	}
}

// TestClaimLeafHash_FieldBoundariesAreRecoverable is the regression test for a
// defect the alignment fixed.
//
// The address, chain id and event id used to be concatenated with no length
// prefix, so the boundaries between them were not recoverable and distinct
// entitlements collided: chain_id="flare" with event_id="evt-1" hashed
// identically to "flareevt" with "-1". chain_id and event_id are gone from the
// leaf entirely now, and the one variable-length field left carries its length.
//
// Two addresses that differ only in where a shared prefix ends must not
// collide.
func TestClaimLeafHash_FieldBoundariesAreRecoverable(t *testing.T) {
	var id [32]byte
	base := ClaimLeaf{
		Pool:         PoolKindContributor,
		IdentityHash: id,
		ClaimAddress: "GABC",
		AmountMinor:  big.NewInt(1),
	}
	longer := base
	longer.ClaimAddress = "GABCD"

	if base.Hash() == longer.Hash() {
		t.Fatal("addresses of different lengths collide; the length prefix is not in the digest")
	}
}

// TestClaimLeafHash_NegativeAmountDoesNotWrap covers the other silent failure
// the old construction had: the amount went through uint64(), so a negative
// became an enormous positive rather than being rejected or zeroed.
func TestClaimLeafHash_NegativeAmountDoesNotWrap(t *testing.T) {
	var id [32]byte
	neg := ClaimLeaf{Pool: PoolKindContributor, IdentityHash: id, ClaimAddress: "GABC", AmountMinor: big.NewInt(-1)}
	zero := ClaimLeaf{Pool: PoolKindContributor, IdentityHash: id, ClaimAddress: "GABC", AmountMinor: big.NewInt(0)}
	huge := ClaimLeaf{Pool: PoolKindContributor, IdentityHash: id, ClaimAddress: "GABC", AmountMinor: new(big.Int).Lsh(big.NewInt(1), 200)}

	if neg.Hash() != zero.Hash() {
		t.Error("a negative amount should render as zero, not as its own value")
	}
	if huge.Hash() == zero.Hash() {
		t.Error("a 200-bit amount collided with zero; 32 bytes should carry it")
	}
}
