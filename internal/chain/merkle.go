package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Merkle claim leaves and roots (on-chain spec §4 and §5.4).
//
// Pure computation with no chain involved, and deliberately so: this is the
// one artefact a contract will depend on byte-for-byte, so it is pinned by a
// golden fixture before anything is deployed against it. If this construction
// changes after a root is published, every proof already served stops
// verifying and the root cannot be corrected.

// Domain-separation prefixes (§9). Without distinct prefixes for leaves and
// internal nodes, a crafted internal node can be presented as a leaf: someone
// who knows two sibling leaves can hash them and submit that digest as their
// own leaf with a proof one level short. The prefixes make the two hash
// domains disjoint.
const (
	leafPrefix byte = 0x00
	nodePrefix byte = 0x01
)

var (
	ErrEmptyTree    = errors.New("cannot build a Merkle tree with no leaves")
	ErrNoSalt       = errors.New("a per-event salt is required")
	ErrLeafNotFound = errors.New("no such leaf in this tree")
)

// IdentityHash is H(github_login_lower || per_event_salt).
//
// **The salt is never published, at any point.** Not in the rules page, not in
// an API response, not in the operations log, not after the event settles.
//
// This is the whole privacy mechanism. Claim transactions already put wallet
// addresses on-chain by their own nature, and the leaf commits to an identity
// hash beside the address. If the salt were ever released, anyone holding a
// list of GitHub logins - which is public - could compute every identity hash,
// match them against the leaves, and read off each contributor's address:
// reconstructing exactly the github_login to wallet mapping §4 exists to
// prevent. That linkage is permanent and, for some contributors, a safety
// issue rather than a preference.
//
// The salt is per event, so leaves cannot be correlated across events even by
// someone who later learns one event's salt.
//
// Lowercasing the login before hashing is load-bearing: GitHub records
// capitalisation inconsistently, and hashing the raw value would give one
// person different identity hashes in different rows - some matching their
// leaf and some not.
func IdentityHash(githubLogin string, salt []byte) ([32]byte, error) {
	if len(salt) == 0 {
		return [32]byte{}, ErrNoSalt
	}
	h := sha256.New()
	h.Write([]byte(strings.ToLower(strings.TrimSpace(githubLogin))))
	h.Write(salt)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// PoolKind is which of an event's two budgets an entitlement is drawn from.
//
// Named PoolKind, not Pool, because this package already uses Pool for one
// chain's sub-pool of one event (pools.go). The word carries two meanings in
// this system - "the Flare sub-pool" and "the contributor budget" - and the
// contract's own enum is the second. Colliding them in Go would make the leaf
// construction read as though it hashed the chain sub-pool, which it does not.
//
// The byte value is inside the leaf, so a contributor leaf cannot verify
// against the maintainer root. §5.1 requires that separation to be structural
// rather than an accounting convention, and the hash is where it becomes
// structural for claims. The values match the contract's enum ordering.
type PoolKind uint8

const (
	PoolKindContributor PoolKind = 0
	PoolKindMaintainer  PoolKind = 1
)

func (p PoolKind) String() string {
	if p == PoolKindMaintainer {
		return "maintainer"
	}
	return "contributor"
}

// ClaimLeaf is one contributor's entitlement in one pool of one event's escrow.
type ClaimLeaf struct {
	// Pool the entitlement is drawn from. Part of the digest.
	Pool PoolKind
	// IdentityHash, never the login itself.
	IdentityHash [32]byte
	// ClaimAddress is the address the contributor registered for this chain,
	// in that chain's canonical string form - a Stellar strkey, an EVM
	// 0x-address. The string is what is hashed, on both sides.
	ClaimAddress string
	// AmountMinor is exact integer minor units (§9): all decimal arithmetic
	// happens off-chain and only integers are published. Arbitrary precision
	// because Soroban accepts i128 and EVM expresses uint256; an int64 here
	// narrowed both and wrapped silently on a negative.
	AmountMinor *big.Int
}

// Hash computes the leaf digest.
//
//	leaf = H( 0x00 || pool || len(address) || address || identity_hash || amount_be32 )
//
// This is the canonical construction, identical in field order and encoding to
// GrainhackEscrow::leaf_hash. It is pinned from both sides against
// testdata/leaf_vector.json; the two implementations drifted once already,
// which is what that vector exists to prevent.
//
// Four decisions worth keeping:
//
// **The pool is in the leaf.** Without it a contributor leaf verifies against
// the maintainer root if the two are ever confused, and §5.1's separation
// stops being structural.
//
// **The address is hashed as its canonical string, length-prefixed.** The
// contract previously hashed Stellar's XDR encoding, which forced every
// off-chain builder to implement XDR - the chain-specific leakage §3.1 exists
// to prevent - and has no meaning at all on EVM. The two-byte length prefix is
// what makes the field boundary recoverable: without it, concatenated
// variable-length fields collide, and this construction demonstrably did.
//
// **The amount is 32-byte big-endian.** Fixed width so "10" and "10.0" cannot
// differ, and wide enough for uint256 so no chain narrows it.
//
// **chain_id and event_id are deliberately absent.** They were in this
// construction and are removed. One deployed escrow is one event on one chain,
// and the root a proof verifies against lives inside that contract - so a leaf
// built for another event or another chain has nowhere to be replayed. Putting
// them back would mean the contract must store and canonicalise two strings it
// cannot otherwise verify, on every chain, to re-derive scoping it already has
// structurally. That cost is the portability this construction is buying. Do
// not re-add them.
func (l ClaimLeaf) Hash() [32]byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write([]byte{byte(l.Pool)})

	addr := []byte(l.ClaimAddress)
	var addrLen [2]byte
	binary.BigEndian.PutUint16(addrLen[:], uint16(len(addr)))
	h.Write(addrLen[:])
	h.Write(addr)

	h.Write(l.IdentityHash[:])
	h.Write(amountBytes32(l.AmountMinor))

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// amountBytes32 renders an amount as 32-byte big-endian, right-aligned.
//
// A nil or negative amount is rendered as zero rather than panicking or
// wrapping: a leaf is built from judged payout rows, and the guard that keeps
// those positive belongs at that boundary. What must never happen here is a
// negative silently becoming an enormous positive, which is exactly what the
// previous uint64 conversion did.
func amountBytes32(v *big.Int) []byte {
	out := make([]byte, 32)
	if v == nil || v.Sign() <= 0 {
		return out
	}
	b := v.Bytes()
	if len(b) > 32 {
		// Unreachable for any real asset; truncating silently would be worse
		// than the most significant bytes being dropped loudly in a test.
		b = b[len(b)-32:]
	}
	copy(out[32-len(b):], b)
	return out
}

// MerkleTree is a built tree with its leaves in canonical order.
type MerkleTree struct {
	Root   [32]byte
	leaves [][32]byte
}

// hashNode hashes a sorted pair with the node prefix. Sorting removes the need
// to transmit left/right position with each proof step; the prefix is what
// keeps that safe.
func hashNode(a, b [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	if bytes.Compare(a[:], b[:]) <= 0 {
		h.Write(a[:])
		h.Write(b[:])
	} else {
		h.Write(b[:])
		h.Write(a[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// BuildMerkleTree hashes and sorts the leaves, then builds the tree.
//
// Leaves are sorted by digest so the same set always produces the same root
// regardless of the order rows came back from the database. A root that
// depended on query order would be irreproducible, which §1 forbids: any
// commitment published on-chain must be recomputable from stored rows.
//
// An odd node at any level is promoted rather than duplicated. Duplicating it
// is the classic CVE-2012-2459 shape, where two different leaf sets produce
// the same root.
func BuildMerkleTree(leaves []ClaimLeaf) (*MerkleTree, error) {
	if len(leaves) == 0 {
		return nil, ErrEmptyTree
	}
	hashed := make([][32]byte, 0, len(leaves))
	for _, l := range leaves {
		hashed = append(hashed, l.Hash())
	}
	return buildFromDigests(hashed)
}

// buildFromDigests is the tree stage on its own, taking leaf digests that have
// already been computed.
//
// Split out so the cross-implementation tree vectors exercise **this** code
// rather than a copy of it. A vector that pins a test-local reimplementation
// pins nothing: it is a second implementation of the same rules, which is
// precisely the drift the vectors exist to catch. The contract's own test
// helper still has that weakness, and is compensated for by claiming through
// the real verifier rather than by asserting a root alone.
//
// The caller passes digests in whatever order it has them; sorting is this
// function's job and is one of the three rules the vectors pin.
func buildFromDigests(hashed [][32]byte) (*MerkleTree, error) {
	if len(hashed) == 0 {
		return nil, ErrEmptyTree
	}
	sorted := make([][32]byte, len(hashed))
	copy(sorted, hashed)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	level := make([][32]byte, len(sorted))
	copy(level, sorted)
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i]) // promote, never duplicate
				continue
			}
			next = append(next, hashNode(level[i], level[i+1]))
		}
		level = next
	}
	return &MerkleTree{Root: level[0], leaves: sorted}, nil
}

// Proof returns the sibling path for one leaf.
func (t *MerkleTree) Proof(leaf ClaimLeaf) ([][32]byte, error) {
	return t.proofForDigest(leaf.Hash())
}

// proofForDigest is Proof by leaf digest, so the pinned proof vector walks the
// same code a real claim does rather than a test-local copy of it.
func (t *MerkleTree) proofForDigest(target [32]byte) ([][32]byte, error) {
	idx := -1
	for i, h := range t.leaves {
		if h == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("%w: %s", ErrLeafNotFound, hex.EncodeToString(target[:]))
	}

	var proof [][32]byte
	level := make([][32]byte, len(t.leaves))
	copy(level, t.leaves)

	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		nextIdx := idx
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				if i == idx {
					nextIdx = len(next)
				}
				next = append(next, level[i])
				continue
			}
			if i == idx {
				proof = append(proof, level[i+1])
				nextIdx = len(next)
			} else if i+1 == idx {
				proof = append(proof, level[i])
				nextIdx = len(next)
			}
			next = append(next, hashNode(level[i], level[i+1]))
		}
		level = next
		idx = nextIdx
	}
	return proof, nil
}

// VerifyProof recomputes a root from a leaf and its proof. Mirrors the
// contract's verification exactly, so the backend can check what it is about
// to publish rather than discovering a mismatch when a contributor cannot
// claim.
func VerifyProof(root, leaf [32]byte, proof [][32]byte) bool {
	computed := leaf
	for _, sib := range proof {
		computed = hashNode(computed, sib)
	}
	return computed == root
}

// TotalMinor sums a leaf set. §9 requires asserting a chain's leaf total
// equals its escrowed amount before publishing that chain's root, and this is
// the number to assert against.
//
// Arbitrary precision for the same reason the leaf carries it: the sum of a
// large event's entitlements is the one number that must not silently wrap.
func TotalMinor(leaves []ClaimLeaf) *big.Int {
	total := new(big.Int)
	for _, l := range leaves {
		if l.AmountMinor != nil {
			total.Add(total, l.AmountMinor)
		}
	}
	return total
}
