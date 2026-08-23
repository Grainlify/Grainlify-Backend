package payout

import (
	"context"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The pool a tree is built for must come from the settlement row, not from a
// constant in the loader.
//
// This is the assertion the acknowledgement gate structurally cannot make.
// InputDigest hashes s.Pool, so the digest looks like it binds the pool - but
// dry-run and build both read the pool from LoadSettlement. When that was a
// hardcoded "contributor", both sides hashed the same constant, agreed with
// each other, and passed, while Build produced a complete and internally
// consistent tree for the wrong pool. An invariant whose two sides come from
// one hardcoded value can only confirm the constant equals itself.
//
// So the check has to be made against something the constant cannot reach: the
// leaf. chain.ClaimLeaf.Hash writes byte(l.Pool) into the digest precisely so a
// contributor leaf cannot verify against a maintainer root. That byte is the
// only place the pool becomes falsifiable, and it is what this asserts.
//
// MUTATION-CHECKED, in two steps, because the first step did not prove what it
// looked like it proved.
//
// Restoring `Pool: "contributor"` in LoadSettlement fails this test - but at
// the s.Pool guard below, which is the cheap assertion, before any leaf is
// examined. That demonstrates the test detects the bug; it says nothing about
// whether the leaf comparison earns its place.
//
// So the guard was disabled as well, leaving only the leaf comparison against
// the hardcode. It failed: the stored leaf did not hash as a maintainer leaf.
// The expensive half is load-bearing on its own, and that is the half that
// corresponds to what actually reaches the chain.
func TestLoadSettlement_BuildsTheTreeForTheStoredPoolNotAConstant(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	const chainID = "aptos-testnet"

	uid := uuid.New()
	p := person{id: uid, login: "maintainer-" + uid.String()[:8], addr: "x", amount: 10_000_000}
	people := []person{p}

	// fixture() writes a contributor settlement, which is the wrong shape here:
	// this test exists to prove the maintainer case, and reusing the fixture
	// would prove the case that was already accidentally correct.
	base := fixture(t, d, chainID, people)
	if _, err := d.Pool.Exec(ctx,
		`UPDATE settlements SET pool = 'maintainer', chain_id = $2 WHERE id = $1`,
		base.SettlementID, chainID); err != nil {
		t.Fatalf("mark settlement as maintainer: %v", err)
	}
	// fixture() returns entitlements in the struct without writing
	// settlement_lines, because every other test here hands that struct
	// straight to Resolve. This test goes through the loader instead, so the
	// rows have to exist - which is itself why the hardcode survived: nothing
	// that reads the database was exercising LoadSettlement at all.
	writeLines(t, d, base.SettlementID, people)

	s, err := LoadSettlement(ctx, d.Pool, base.SettlementID, chainID)
	if err != nil {
		t.Fatalf("LoadSettlement: %v", err)
	}
	if s.Pool != "maintainer" {
		t.Fatalf("LoadSettlement returned pool %q for a settlement stored as maintainer; "+
			"the tree would be built for the wrong pool and nothing downstream would error", s.Pool)
	}

	rep, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{
		InputDigest:        rep.InputDigest,
		ExcludedTotalMinor: rep.ExcludedTotalMinor,
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Read the leaf back as stored. identity_hash is read rather than
	// recomputed, per the standing rule in build.go: a rename changes it and
	// the root is permanent.
	var leafHash, identity []byte
	var addr string
	var amount int64
	if err := d.Pool.QueryRow(ctx, `
		SELECT leaf_hash, identity_hash, claim_address, amount_minor
		FROM claim_leaves WHERE settlement_id = $1 AND leaf_index = 0`,
		s.SettlementID).Scan(&leafHash, &identity, &addr, &amount); err != nil {
		t.Fatalf("read leaf: %v", err)
	}

	var id32 [32]byte
	copy(id32[:], identity)
	leafFor := func(pk chain.PoolKind) [32]byte {
		return chain.ClaimLeaf{
			Pool:         pk,
			IdentityHash: id32,
			ClaimAddress: addr,
			AmountMinor:  big.NewInt(amount),
		}.Hash()
	}

	wantMaintainer := leafFor(chain.PoolKindMaintainer)
	if string(leafHash) != string(wantMaintainer[:]) {
		t.Fatalf("stored leaf does not hash as a maintainer leaf; the tree was built for another pool")
	}

	// The complement of the check above, kept because the two fail for
	// different reasons: the first says the leaf is not what it should be, this
	// says it is specifically the other pool's leaf. Under the restored
	// hardcode the first one fires; this one is what distinguishes "built for
	// the wrong pool" from "built wrong somehow".
	wrong := leafFor(chain.PoolKindContributor)
	if string(leafHash) == string(wrong[:]) {
		t.Fatal("stored leaf hashes as a CONTRIBUTOR leaf for a settlement recorded as maintainer: " +
			"the tree is valid, complete, and pays the wrong pool")
	}
}

// A chain named by the caller that disagrees with the stored one is refused,
// not resolved in either direction.
//
// chain_id is hashed into the digest and decides which escrow is funded.
// Preferring the argument would build a tree for a chain the settlement does
// not name; preferring the row would silently ignore what the operator typed.
// Both are worse than stopping.
func TestLoadSettlement_RefusesAChainThatContradictsTheRow(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	uid := uuid.New()
	base := fixture(t, d, "aptos-testnet", []person{
		{id: uid, login: "chain-" + uid.String()[:8], addr: "x", amount: 10_000_000},
	})
	if _, err := d.Pool.Exec(ctx,
		`UPDATE settlements SET chain_id = 'aptos-testnet' WHERE id = $1`, base.SettlementID); err != nil {
		t.Fatalf("set chain: %v", err)
	}
	writeLines(t, d, base.SettlementID, []person{{id: uid, amount: 10_000_000}})

	if _, err := LoadSettlement(ctx, d.Pool, base.SettlementID, "aptos-mainnet"); err == nil {
		t.Fatal("loaded a settlement recorded on aptos-testnet while aptos-mainnet was requested")
	}

	// An empty argument is not a contradiction: it is the caller declining to
	// assert, and the row answers.
	s, err := LoadSettlement(ctx, d.Pool, base.SettlementID, "")
	if err != nil {
		t.Fatalf("LoadSettlement with no chain asserted: %v", err)
	}
	if s.ChainID != "aptos-testnet" {
		t.Fatalf("ChainID = %q, want the stored aptos-testnet", s.ChainID)
	}
}

// writeLines persists the settlement_lines a loader needs.
func writeLines(t *testing.T, d *db.DB, sid uuid.UUID, people []person) {
	t.Helper()
	ctx := context.Background()
	for _, p := range people {
		if _, err := d.Pool.Exec(ctx, `
			INSERT INTO settlement_lines
			  (settlement_id, user_id, raw_weight, multiplier, effective_weight, usdc_amount, amount_minor)
			VALUES ($1, $2, 1, 1, 1, 10, $3)`, sid, p.id, p.amount); err != nil {
			t.Fatalf("settlement line for %s: %v", p.id, err)
		}
	}
	t.Cleanup(func() {
		d.Pool.Exec(ctx, `DELETE FROM settlement_lines WHERE settlement_id = $1`, sid)
	})
}
