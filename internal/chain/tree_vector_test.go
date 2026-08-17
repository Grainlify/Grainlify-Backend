package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// Cross-implementation vectors for the TREE, as distinct from the leaf.
//
// The leaf digest has been pinned across implementations since the two sides
// drifted; the tree above it was not, and that gap was measured rather than
// suspected. With the whole Go suite passing, all three of these mutations
// survived:
//
//	nodePrefix deleted             - no test failed
//	nodePrefix 0x01 -> 0x09        - no test failed
//	leaf sort reversed to descending - no test failed
//
// Every internal-node rule was therefore free to differ between this builder
// and a verifier in another language while both suites stayed green. The first
// symptom would have been a contributor unable to claim against a root that
// cannot be corrected.
//
// These tests close that. The same roots are asserted from the contract side
// in Stellar-Contracts/contracts/grainhack-escrow/src/test.rs, and the Move
// verifier will be written against them rather than against this package.

// synthLeaf is the vector file's documented generator:
//
//	leaf_i = sha256( 0xFF || uint16_be(i) )
//
// Synthetic digests, so the tree vectors pin the tree rule alone and stay
// valid on any chain whatever its address encoding. 0xFF is neither the leaf
// prefix nor the node prefix, so one can never collide with a real leaf.
func synthLeaf(i int) [32]byte {
	var b [3]byte
	b[0] = 0xFF
	binary.BigEndian.PutUint16(b[1:], uint16(i))
	return sha256.Sum256(b[:])
}

// synthLeaves returns n digests in generator order, deliberately NOT sorted.
// Sorting is one of the rules under test; handing the builder a pre-sorted
// list would leave it untested.
func synthLeaves(n int) [][32]byte {
	out := make([][32]byte, n)
	for i := range out {
		out[i] = synthLeaf(i)
	}
	return out
}

func mustHex(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad 32-byte hex %q: %v", s, err)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// TestTreeVector_GeneratorMatchesTheVector checks the synthetic inputs before
// anything is built from them.
//
// Without this, a divergence in the generator would surface as every root
// being wrong, which reads as a tree bug and sends the reader to the wrong
// file.
func TestTreeVector_GeneratorMatchesTheVector(t *testing.T) {
	v := loadLeafVector(t)
	if len(v.TreeVectors.LeavesSample) == 0 {
		t.Fatal("vector file has no leaves_sample; the generator is unanchored")
	}
	for idx, want := range v.TreeVectors.LeavesSample {
		i := atoiOrFail(t, idx)
		got := synthLeaf(i)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("leaf_%d\n got  %s\n want %s", i, hex.EncodeToString(got[:]), want)
		}
	}
}

// TestTreeVector_RootsMatchAcrossImplementations is the pin.
//
// The counts are chosen, not arbitrary. Because sibling pairs are sorted
// inside the node hash, reversing the leaf sort is invisible at power-of-two
// counts and changes the root at every other count:
//
//	n=2 identical   n=3 DIFFERENT   n=4 identical   n=5 DIFFERENT
//	n=6 DIFFERENT   n=7 DIFFERENT   n=8 identical
//
// So 3, 5, 6 and 7 are here because they are the counts that can catch it.
// 1 and 2 anchor the degenerate cases. 38 is the real founding contributor
// pool size - 37 members plus the one backfilled by migration 000071 - and is
// the first tree intended for publication.
//
// If this fails, one implementation moved. Fix the side that moved; do not
// update the vector to make it pass. A root that has been published cannot be
// corrected, so a vector edited to match a changed builder is a claim nobody
// can make.
func TestTreeVector_RootsMatchAcrossImplementations(t *testing.T) {
	v := loadLeafVector(t)
	if len(v.TreeVectors.Roots) == 0 {
		t.Fatal("vector file has no tree roots; the tree is unpinned")
	}

	for countStr, want := range v.TreeVectors.Roots {
		n := atoiOrFail(t, countStr)
		tree, err := buildFromDigests(synthLeaves(n))
		if err != nil {
			t.Fatalf("n=%d: build: %v", n, err)
		}
		if got := hex.EncodeToString(tree.Root[:]); got != want {
			t.Errorf("n=%d root\n got  %s\n want %s", n, got, want)
		}
	}
}

// TestTreeVector_PinnedProofVerifies cross-checks the sibling path, not merely
// the root.
//
// A root vector alone proves the two sides agree on the final digest. It does
// not prove they agree on the path a claimant submits to get there, and the
// path is what actually travels between the backend and the contract. n=7 is
// the deepest odd tree in the vector, so this path crosses a promoted node -
// the case a duplicating implementation gets wrong.
func TestTreeVector_PinnedProofVerifies(t *testing.T) {
	v := loadLeafVector(t)
	p := v.TreeVectors.Proof

	tree, err := buildFromDigests(synthLeaves(p.LeafCount))
	if err != nil {
		t.Fatalf("build n=%d: %v", p.LeafCount, err)
	}

	leaf := mustHex(t, p.Leaf)
	root := mustHex(t, p.ExpectedRoot)

	if tree.Root != root {
		t.Fatalf("tree root disagrees with the pinned proof root\n got  %x\n want %x", tree.Root, root)
	}

	// The path this builder produces must be the pinned one, sibling for
	// sibling. Two different paths can both fold to the right root, so
	// comparing only the outcome would let the orderings drift apart.
	got, err := tree.proofForDigest(leaf)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if len(got) != len(p.Siblings) {
		t.Fatalf("proof length = %d, want %d", len(got), len(p.Siblings))
	}
	for i, want := range p.Siblings {
		if hex.EncodeToString(got[i][:]) != want {
			t.Errorf("sibling %d\n got  %s\n want %s", i, hex.EncodeToString(got[i][:]), want)
		}
	}

	if !VerifyProof(root, leaf, got) {
		t.Error("the pinned proof does not verify against the pinned root")
	}
}

// TestMerkleTree_NodePrefixIsInTheDigest names the rule directly, so a failure
// says which one broke rather than only that some root moved.
//
// The prefix is what makes the leaf and node hash domains disjoint. Without
// it, somebody who knows two sibling leaves can hash them and submit that
// digest as their own leaf with a proof one level short - the contract's
// an_internal_node_cannot_be_claimed_as_a_leaf covers the same property from
// the other side.
func TestMerkleTree_NodePrefixIsInTheDigest(t *testing.T) {
	a, b := synthLeaf(0), synthLeaf(1)

	lo, hi := a, b
	if bytes.Compare(lo[:], hi[:]) > 0 {
		lo, hi = hi, lo
	}
	unprefixed := sha256.Sum256(append(append([]byte{}, lo[:]...), hi[:]...))

	if hashNode(a, b) == unprefixed {
		t.Fatal("an internal node hashes as the bare pair; the node prefix is not in the digest")
	}
}

// TestMerkleTree_LeavesAreSortedAscending names the second rule.
//
// Ascending specifically, not merely "sorted": descending is also a total
// order and also produces a reproducible root, but a different one at every
// non-power-of-two leaf count. Two implementations that each sorted
// consistently in opposite directions would both look correct in isolation.
func TestMerkleTree_LeavesAreSortedAscending(t *testing.T) {
	tree, err := buildFromDigests(synthLeaves(7))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for i := 1; i < len(tree.leaves); i++ {
		if bytes.Compare(tree.leaves[i-1][:], tree.leaves[i][:]) >= 0 {
			t.Fatalf("leaves are not in ascending digest order at index %d", i)
		}
	}
}

// TestMerkleTree_InputOrderDoesNotReachTheRoot is the property the sort exists
// to provide: a root must not depend on the order rows came back from the
// database, or it cannot be recomputed from stored rows months later.
//
// Distinct from TestBuildMerkleTree_RootIsOrderIndependent, which covers the
// same ground at the ClaimLeaf level with three leaves. This runs at every
// pinned count, including the ones where promotion picks a different element.
func TestMerkleTree_InputOrderDoesNotReachTheRoot(t *testing.T) {
	for _, n := range []int{3, 5, 6, 7, 38} {
		forward := synthLeaves(n)
		reversed := make([][32]byte, n)
		for i, d := range forward {
			reversed[n-1-i] = d
		}

		a, err := buildFromDigests(forward)
		if err != nil {
			t.Fatalf("n=%d forward: %v", n, err)
		}
		b, err := buildFromDigests(reversed)
		if err != nil {
			t.Fatalf("n=%d reversed: %v", n, err)
		}
		if a.Root != b.Root {
			t.Errorf("n=%d: root depends on input order\n forward  %x\n reversed %x", n, a.Root, b.Root)
		}
	}
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("vector key %q is not a leaf count", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
