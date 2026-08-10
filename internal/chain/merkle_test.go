package chain

import (
	"encoding/hex"
	"testing"
)

// goldenSalt is a fixed *test* salt, so the leaf format is pinned without a
// real per-event salt ever entering the repository. A real salt is generated
// per event, stored encrypted, and never published anywhere - see
// IdentityHash's documentation for why releasing one would undo the whole
// privacy mechanism.
var goldenSalt = []byte("grainhack-golden-fixture-salt-do-not-use-in-production")

func leafFor(t *testing.T, login, addr string, amount int64, chainID, eventID string) ClaimLeaf {
	t.Helper()
	id, err := IdentityHash(login, goldenSalt)
	if err != nil {
		t.Fatalf("IdentityHash: %v", err)
	}
	return ClaimLeaf{
		IdentityHash: id, ClaimAddress: addr, AmountMinor: amount,
		ChainID: chainID, EventID: eventID,
	}
}

// The golden fixture. A contract will depend on this construction
// byte-for-byte, and once a root is published a change here invalidates every
// proof already served - with no way to correct the root.
//
// If this test fails, the leaf format changed. That is a decision to make
// deliberately and coordinate with the contract, never a fixture to update
// until it passes.
func TestClaimLeaf_GoldenFixture(t *testing.T) {
	id, err := IdentityHash("Octocat", goldenSalt)
	if err != nil {
		t.Fatalf("IdentityHash: %v", err)
	}
	const wantIdentity = "7cca2f153c795f51de4b282f9c39ea38ef5b2f23d4ba873ace686e60d92f8822"
	if got := hex.EncodeToString(id[:]); got != wantIdentity {
		t.Errorf("identity hash construction changed:\n  got  %s\n  want %s", got, wantIdentity)
	}

	leaf := ClaimLeaf{
		IdentityHash: id,
		ClaimAddress: "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		AmountMinor:  1_500_000,
		ChainID:      "soroban",
		EventID:      "event-1",
	}
	h := leaf.Hash()
	const wantLeaf = "e9ff2a2093d251a3a3b1bcecbc84e39af66959576b547ead2f4521843d9be402"
	if got := hex.EncodeToString(h[:]); got != wantLeaf {
		t.Errorf("leaf construction changed:\n  got  %s\n  want %s\n"+
			"This is the value a deployed contract verifies against. Changing it invalidates "+
			"every proof already served against a published root, and the root cannot be corrected.", got, wantLeaf)
	}

	// Stability is the property under test: the same inputs must always
	// produce the same digest, within a run and across releases.
	if leaf.Hash() != h {
		t.Fatal("leaf hashing is not deterministic")
	}

	// Every field is bound into the hash. If any of these collide, that field
	// could be altered without invalidating a proof.
	variants := map[string]ClaimLeaf{
		"different amount":  {IdentityHash: id, ClaimAddress: leaf.ClaimAddress, AmountMinor: 1_500_001, ChainID: "soroban", EventID: "event-1"},
		"different address": {IdentityHash: id, ClaimAddress: "GBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", AmountMinor: 1_500_000, ChainID: "soroban", EventID: "event-1"},
		"different chain":   {IdentityHash: id, ClaimAddress: leaf.ClaimAddress, AmountMinor: 1_500_000, ChainID: "flare", EventID: "event-1"},
		"different event":   {IdentityHash: id, ClaimAddress: leaf.ClaimAddress, AmountMinor: 1_500_000, ChainID: "soroban", EventID: "event-2"},
	}
	for name, v := range variants {
		if v.Hash() == h {
			t.Errorf("%s produced an identical leaf; that field is not bound into the hash", name)
		}
	}
}

// The same login in two different events must produce unrelated leaves.
//
// The salt is per event precisely so that someone who somehow learned one
// event's salt still could not correlate a contributor's leaves across
// events - each event's identity hashes are independent.
func TestIdentityHash_SameLoginIsUnrelatedAcrossEvents(t *testing.T) {
	saltA := []byte("event-a-salt")
	saltB := []byte("event-b-salt")

	a, err := IdentityHash("octocat", saltA)
	if err != nil {
		t.Fatalf("IdentityHash: %v", err)
	}
	b, err := IdentityHash("octocat", saltB)
	if err != nil {
		t.Fatalf("IdentityHash: %v", err)
	}
	if a == b {
		t.Fatal("the same login produced the same identity hash in two events; leaves would be correlatable across events")
	}

	// And the resulting leaves differ too, even with identical address and
	// amount.
	la := ClaimLeaf{IdentityHash: a, ClaimAddress: "GADDR", AmountMinor: 100, ChainID: "soroban", EventID: "event-a"}
	lb := ClaimLeaf{IdentityHash: b, ClaimAddress: "GADDR", AmountMinor: 100, ChainID: "soroban", EventID: "event-b"}
	if la.Hash() == lb.Hash() {
		t.Error("leaves for the same contributor in two events are identical")
	}
}

// A contributor earning on two chains has two independent leaves. Nothing is
// aggregated across chains (§5.4), and one chain's leaf must never verify
// against another chain's root.
func TestClaimLeaf_PerChainLeavesAreIndependent(t *testing.T) {
	soroban := leafFor(t, "octocat", "GADDR", 500, "soroban", "event-1")
	flare := leafFor(t, "octocat", "0xADDR", 500, "flare", "event-1")

	if soroban.Hash() == flare.Hash() {
		t.Fatal("the same contributor's leaves on two chains are identical")
	}

	// Even with the identical registered address - which is unrealistic, but
	// it isolates chain_id as the binding field.
	sameAddr := leafFor(t, "octocat", "GADDR", 500, "flare", "event-1")
	if soroban.Hash() == sameAddr.Hash() {
		t.Error("chain_id is not bound into the leaf; a Soroban leaf could be replayed against a Flare root")
	}

	// A tree per chain, and neither leaf verifies against the other's root.
	sorobanTree, err := BuildMerkleTree([]ClaimLeaf{soroban})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	flareTree, err := BuildMerkleTree([]ClaimLeaf{flare})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if VerifyProof(flareTree.Root, soroban.Hash(), nil) {
		t.Error("a Soroban leaf verified against the Flare root")
	}
	if VerifyProof(sorobanTree.Root, flare.Hash(), nil) {
		t.Error("a Flare leaf verified against the Soroban root")
	}
}

func TestMerkleTree_ProofsVerifyAndForgeriesDoNot(t *testing.T) {
	var leaves []ClaimLeaf
	for _, l := range []struct {
		login  string
		addr   string
		amount int64
	}{
		{"alice", "GA1", 100}, {"bob", "GB2", 250}, {"carol", "GC3", 75},
		{"dave", "GD4", 400}, {"erin", "GE5", 175},
	} {
		leaves = append(leaves, leafFor(t, l.login, l.addr, l.amount, "soroban", "event-1"))
	}

	tree, err := BuildMerkleTree(leaves)
	if err != nil {
		t.Fatalf("BuildMerkleTree: %v", err)
	}

	for _, l := range leaves {
		proof, err := tree.Proof(l)
		if err != nil {
			t.Fatalf("Proof: %v", err)
		}
		if !VerifyProof(tree.Root, l.Hash(), proof) {
			t.Errorf("a valid proof did not verify for %s", l.ClaimAddress)
		}
		// The same proof must not verify a tampered amount.
		tampered := l
		tampered.AmountMinor = l.AmountMinor + 1
		if VerifyProof(tree.Root, tampered.Hash(), proof) {
			t.Errorf("a proof verified an inflated amount for %s", l.ClaimAddress)
		}
	}

	// A leaf that is not in the tree has no proof.
	outsider := leafFor(t, "mallory", "GM9", 999, "soroban", "event-1")
	if _, err := tree.Proof(outsider); err == nil {
		t.Error("produced a proof for a leaf that is not in the tree")
	}
}

// The root must not depend on the order rows came back from the database.
// §1 requires every on-chain artefact to be recomputable from stored rows,
// and a root that changed with query order would not be.
func TestBuildMerkleTree_RootIsOrderIndependent(t *testing.T) {
	a := leafFor(t, "alice", "GA1", 100, "soroban", "event-1")
	b := leafFor(t, "bob", "GB2", 200, "soroban", "event-1")
	c := leafFor(t, "carol", "GC3", 300, "soroban", "event-1")

	one, err := BuildMerkleTree([]ClaimLeaf{a, b, c})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	two, err := BuildMerkleTree([]ClaimLeaf{c, a, b})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if one.Root != two.Root {
		t.Error("the root changed with input order; it would not be reproducible from stored rows")
	}
}

// An odd node is promoted, not duplicated. Duplicating it is the
// CVE-2012-2459 shape, where two different leaf sets yield the same root.
func TestBuildMerkleTree_OddNodeIsPromotedNotDuplicated(t *testing.T) {
	a := leafFor(t, "alice", "GA1", 100, "soroban", "event-1")
	b := leafFor(t, "bob", "GB2", 200, "soroban", "event-1")
	c := leafFor(t, "carol", "GC3", 300, "soroban", "event-1")

	three, err := BuildMerkleTree([]ClaimLeaf{a, b, c})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The duplicate-the-odd-node bug would make this four-leaf set collide
	// with the three-leaf one.
	four, err := BuildMerkleTree([]ClaimLeaf{a, b, c, c})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if three.Root == four.Root {
		t.Error("two different leaf sets produced the same root")
	}
}

// An empty salt is refused rather than silently hashing the bare login, which
// would make every identity hash trivially computable from a public login list.
func TestIdentityHash_RefusesAnEmptySalt(t *testing.T) {
	if _, err := IdentityHash("octocat", nil); err == nil {
		t.Error("an empty salt was accepted; identity hashes would be computable from public logins alone")
	}
}

// Login capitalisation must not change the identity hash: GitHub records it
// inconsistently, and one person would otherwise get a hash that matches their
// leaf in some rows and not others.
func TestIdentityHash_IsCaseInsensitive(t *testing.T) {
	a, _ := IdentityHash("OctoCat", goldenSalt)
	b, _ := IdentityHash("octocat", goldenSalt)
	c, _ := IdentityHash("  octocat  ", goldenSalt)
	if a != b || b != c {
		t.Error("identity hash depends on capitalisation or surrounding whitespace")
	}
}

func TestTotalMinor_SumsForTheEscrowAssertion(t *testing.T) {
	leaves := []ClaimLeaf{
		leafFor(t, "alice", "GA1", 100, "soroban", "e"),
		leafFor(t, "bob", "GB2", 250, "soroban", "e"),
	}
	if got := TotalMinor(leaves); got != 350 {
		t.Errorf("TotalMinor = %d, want 350", got)
	}
}
