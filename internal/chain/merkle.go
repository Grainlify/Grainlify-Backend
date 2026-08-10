package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
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

// ClaimLeaf is one contributor's entitlement on one chain for one event.
type ClaimLeaf struct {
	// IdentityHash, never the login itself.
	IdentityHash [32]byte
	// ClaimAddress is the address the contributor registered for this chain.
	ClaimAddress string
	// AmountMinor is exact integer minor units (§9): all decimal arithmetic
	// happens off-chain and only integers are published.
	AmountMinor int64
	ChainID     string
	EventID     string
}

// Hash computes the leaf digest.
//
//	leaf = H( 0x00 || identity_hash || claim_address || amount_be || chain_id || event_id )
//
// chain_id and event_id are inside the hash so a leaf is bound to one chain of
// one event. Without them, a leaf from a contributor's Soroban entitlement
// could be replayed against their Flare root, or against the same chain in a
// later event - and §2 requires that money never moves between chains for any
// reason.
//
// The amount is fixed-width big-endian rather than a decimal string, because
// "10" and "10.0" would otherwise produce different leaves for the same
// entitlement.
func (l ClaimLeaf) Hash() [32]byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(l.IdentityHash[:])
	h.Write([]byte(l.ClaimAddress))
	var amt [8]byte
	binary.BigEndian.PutUint64(amt[:], uint64(l.AmountMinor))
	h.Write(amt[:])
	h.Write([]byte(l.ChainID))
	h.Write([]byte(l.EventID))
	var out [32]byte
	copy(out[:], h.Sum(nil))
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
	sort.Slice(hashed, func(i, j int) bool {
		return bytes.Compare(hashed[i][:], hashed[j][:]) < 0
	})

	level := make([][32]byte, len(hashed))
	copy(level, hashed)
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
	return &MerkleTree{Root: level[0], leaves: hashed}, nil
}

// Proof returns the sibling path for one leaf.
func (t *MerkleTree) Proof(leaf ClaimLeaf) ([][32]byte, error) {
	target := leaf.Hash()
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
func TotalMinor(leaves []ClaimLeaf) int64 {
	var total int64
	for _, l := range leaves {
		total += l.AmountMinor
	}
	return total
}
