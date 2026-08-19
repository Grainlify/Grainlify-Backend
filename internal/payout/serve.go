package payout

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

var ErrNoClaim = errors.New("no claim for this address in this settlement")

// Claim is everything a contributor needs to call the contract, and nothing
// about who they are.
type Claim struct {
	SettlementID uuid.UUID
	LeafIndex    int
	LeafHash     [32]byte
	IdentityHash [32]byte
	ClaimAddress string
	AmountMinor  int64
	Proof        [][32]byte
	Root         [32]byte
}

// ClaimFor returns the leaf and proof for one address.
//
// # This is the function the cold-salt design exists for
//
// It touches no salt. Not "does not need one in the common case" - there is no
// code path here that could decrypt one, because everything it returns was
// persisted at build time.
//
// That is the whole argument for claim_leaves. If a proof required rebuilding
// the tree, the salt could never be destroyed, and a destruction that leaves the
// system unable to serve a claim is not a destruction anybody would perform.
//
// Lookup is by ADDRESS, deliberately. Not by user id: a claim_leaves row keyed to
// a person would put the login-to-leaf mapping the salt protects into the table
// in plaintext, and salt destruction would be theatre. The contributor proves
// control of the address by holding its key, which they must do to claim anyway.
//
// # identity_hash is returned, never recomputed
//
// The stored value is what the leaf commits to. Recomputing H(login || salt) here
// would need the salt, and after a GitHub rename it would produce a different
// answer for the same person - failing for everyone who renamed and passing for
// everyone who did not, which reads as data corruption and is not.
func ClaimFor(ctx context.Context, pool db.DBPool, settlementID uuid.UUID, address string) (*Claim, error) {
	addr, err := payoutaddr.Validate(address)
	if err != nil {
		return nil, fmt.Errorf("payout.ClaimFor: %w", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT leaf_index, leaf_hash, identity_hash, claim_address, amount_minor
		FROM claim_leaves WHERE settlement_id = $1 ORDER BY leaf_index`, settlementID)
	if err != nil {
		return nil, fmt.Errorf("payout.ClaimFor: load leaves: %w", err)
	}
	defer rows.Close()

	type row struct {
		idx         int
		leaf, ident []byte
		addr        string
		amount      int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.idx, &r.leaf, &r.ident, &r.addr, &r.amount); err != nil {
			return nil, fmt.Errorf("payout.ClaimFor: scan: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: settlement %s has no leaves", ErrNoClaim, settlementID)
	}

	// Rebuild the tree from STORED DIGESTS. buildFromDigests is the same code
	// the root was produced by, so a proof served here verifies against the root
	// on chain by construction rather than by coincidence.
	digests := make([][32]byte, len(all))
	target := -1
	for i, r := range all {
		copy(digests[i][:], r.leaf)
		if r.addr == addr {
			target = i
		}
	}
	if target < 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoClaim, addr)
	}

	tree, err := chain.BuildFromDigests(digests)
	if err != nil {
		return nil, fmt.Errorf("payout.ClaimFor: rebuild tree: %w", err)
	}
	proof, err := tree.ProofForDigest(digests[target])
	if err != nil {
		return nil, fmt.Errorf("payout.ClaimFor: proof: %w", err)
	}

	// Verify against the PUBLISHED root, not the one just recomputed.
	//
	// Checking a proof against a root derived from the same rows it was built
	// from establishes only internal consistency, which was never in doubt: a
	// corrupted claim_leaves row yields a perfectly self-consistent tree that
	// simply has a different root, and the check passes. A mutation deleting the
	// verification survived the whole suite, which is what surfaced this.
	//
	// The published root is the authority - the same rule the reconciler works
	// to, applied at the moment a proof is handed to somebody.
	var published []byte
	if err := pool.QueryRow(ctx,
		`SELECT root FROM payout_event_roots WHERE settlement_id = $1`, settlementID).Scan(&published); err != nil {
		return nil, fmt.Errorf("payout.ClaimFor: no published root for settlement %s: %w", settlementID, err)
	}
	var root [32]byte
	copy(root[:], published)

	c := &Claim{
		SettlementID: settlementID,
		LeafIndex:    all[target].idx,
		ClaimAddress: all[target].addr,
		AmountMinor:  all[target].amount,
		Proof:        proof,
		Root:         root,
	}
	copy(c.LeafHash[:], all[target].leaf)
	copy(c.IdentityHash[:], all[target].ident)

	if tree.Root != root {
		return nil, fmt.Errorf("payout.ClaimFor: claim_leaves no longer reproduces the published root for "+
			"settlement %s (stored leaves give %x, published %x). The rows have drifted from what was "+
			"published; a proof built from them would abort on chain and read as the contract rejecting "+
			"the claimant", settlementID, tree.Root, root)
	}

	// A proof that does not verify is worse than no proof: the contributor takes
	// it to the chain, the transaction aborts, and the failure reads as the
	// contract rejecting them rather than as our data being wrong.
	if !chain.VerifyProof(c.Root, c.LeafHash, c.Proof) {
		return nil, fmt.Errorf("payout.ClaimFor: built a proof that does not verify against the published "+
			"root; refusing to serve it for settlement %s", settlementID)
	}
	return c, nil
}
