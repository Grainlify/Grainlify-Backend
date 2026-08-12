package chain

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// leafVector is the cross-implementation pin shared with the Soroban contract.
// See testdata/leaf_vector.json for what each field means and why two digests
// are currently recorded rather than one.
type leafVector struct {
	Inputs struct {
		IdentityHashHex string `json:"identity_hash_hex"`
		ClaimAddress    string `json:"claim_address"`
		AmountMinor     int64  `json:"amount_minor"`
		ChainID         string `json:"chain_id"`
		EventID         string `json:"event_id"`
	} `json:"inputs"`
	ContractLeafContributor string `json:"contract_leaf_contributor"`
	BackendLeafCurrent      string `json:"backend_leaf_current"`
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

func (v leafVector) leaf(t *testing.T) ClaimLeaf {
	t.Helper()
	idBytes, err := hex.DecodeString(v.Inputs.IdentityHashHex)
	if err != nil || len(idBytes) != 32 {
		t.Fatalf("bad identity hash in vector: %v", err)
	}
	var id [32]byte
	copy(id[:], idBytes)
	return ClaimLeaf{
		IdentityHash: id,
		ClaimAddress: v.Inputs.ClaimAddress,
		AmountMinor:  v.Inputs.AmountMinor,
		ChainID:      v.Inputs.ChainID,
		EventID:      v.Inputs.EventID,
	}
}

// TestClaimLeafHash_MatchesPinnedVector pins what this builder produces today.
//
// It exists so a change to the construction cannot be silent. The same vector
// is asserted from the contract side, so whichever implementation moves, one of
// the two tests goes red.
func TestClaimLeafHash_MatchesPinnedVector(t *testing.T) {
	v := loadLeafVector(t)
	got := v.leaf(t).Hash()
	if hex.EncodeToString(got[:]) != v.BackendLeafCurrent {
		t.Errorf("leaf digest changed\n got  %s\n want %s\nUpdate testdata/leaf_vector.json AND the copy in the contract tests deliberately, never to make a test pass",
			hex.EncodeToString(got[:]), v.BackendLeafCurrent)
	}
}

// TestClaimLeafHash_DivergesFromTheContract records the defect rather than
// hiding it.
//
// The contract exports leaf() precisely so this builder could be checked
// against it; that was never wired up, and the two constructions drifted. They
// hash different field sets - the contract binds the pool and encodes the
// address as XDR, this binds chain and event ids and encodes the address as
// UTF-8 - so no choice of inputs makes them agree.
//
// A root built here cannot be claimed against the deployed contract. Nothing
// in production depends on it yet: claim_leaves does not exist and no chain
// pool has ever been created.
//
// **This test is expected to fail once the two are aligned.** When it does,
// delete it and assert equality against the single canonical digest instead.
// It is written as an assertion rather than a comment so alignment cannot be
// declared done while the digests still differ.
func TestClaimLeafHash_DivergesFromTheContract(t *testing.T) {
	v := loadLeafVector(t)
	got := v.leaf(t).Hash()
	if hex.EncodeToString(got[:]) == v.ContractLeafContributor {
		t.Fatal("the builder now agrees with the contract - alignment has landed; replace this test with an equality assertion against the canonical digest and drop backend_leaf_current from the vector")
	}
}

// TestClaimLeafHash_ConcatenationIsAmbiguous demonstrates a second defect in
// the current construction, independent of the divergence.
//
// The address, chain id and event id are variable-length and concatenated with
// no length prefix or separator, so the boundaries between them are not
// recoverable from the digest input. Two different entitlements therefore hash
// to the same leaf.
//
// Not currently exploitable - chain ids come from a fixed internal set and
// event ids are UUIDs - but it is a canonicalisation bug, and the fix (length
// prefixes) belongs in the same change that aligns the two constructions.
func TestClaimLeafHash_ConcatenationIsAmbiguous(t *testing.T) {
	base := ClaimLeaf{AmountMinor: 1, ChainID: "flare", EventID: "evt-1"}
	shifted := base
	// One character moved across the boundary: "flare"+"evt-1" and
	// "flareevt"+"-1" are different entitlements with the same digest input.
	shifted.ChainID = "flareevt"
	shifted.EventID = "-1"

	if base.Hash() != shifted.Hash() {
		t.Skip("construction now disambiguates its fields; fold this into the canonical test")
	}
	t.Log("confirmed: chain_id and event_id boundaries are not recoverable, so distinct entitlements collide")
}
