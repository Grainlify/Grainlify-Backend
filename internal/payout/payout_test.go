package payout

import (
	"context"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

const testKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func saltKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(b)
}

type person struct {
	id     uuid.UUID
	login  string
	addr   string
	amount int64
	reason string
}

func fixture(t *testing.T, d *db.DB, chainID string, people []person) Settlement {
	t.Helper()
	ctx := context.Background()
	var sid uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO founding_settlements (pool_usdc, total_shares, share_value_usdc, pool_minor, asset_decimals)
		VALUES (1,1,1,$1,6) RETURNING id`, 10_000_000).Scan(&sid); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	ents := make([]Entitlement, 0, len(people))
	for _, p := range people {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO users (id, role) VALUES ($1,'contributor')`, p.id); err != nil {
			t.Fatalf("user: %v", err)
		}
		if p.login != "" {
			if _, err := d.Pool.Exec(ctx, `
				INSERT INTO github_accounts (id, user_id, github_user_id, login, access_token, created_at, updated_at)
				VALUES (gen_random_uuid(), $1, $2, $3, '\x00'::bytea, now(), now())`,
				p.id, int64(uuid.New().ID()), p.login); err != nil {
				t.Fatalf("github account: %v", err)
			}
		}
		if p.addr != "" {
			if _, err := d.Pool.Exec(ctx, `
				INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
				VALUES ($1,$2,$3,'nonce')`, p.id, chainID, p.addr); err != nil {
				t.Fatalf("address: %v", err)
			}
		}
		ents = append(ents, Entitlement{UserID: p.id, AmountMinor: big.NewInt(p.amount), IneligibleReason: p.reason})
	}
	t.Cleanup(func() {
		d.Pool.Exec(ctx, `DELETE FROM claim_leaves WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_roots WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_salts WHERE settlement_id=$1`, sid)
		for _, p := range people {
			d.Pool.Exec(ctx, `DELETE FROM contributor_addresses WHERE user_id=$1`, p.id)
			d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id=$1`, p.id)
			d.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, p.id)
		}
		d.Pool.Exec(ctx, `DELETE FROM founding_settlements WHERE id=$1`, sid)
	})
	return Settlement{
		SettlementID: sid, ChainID: chainID, PoolMinor: big.NewInt(10_000_000),
		AssetDecimals: 6, Pool: "contributor", Entitlements: ents,
	}
}

const addrA = "0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9"
const addrB = "0xb33bd154899ec9207b25221821265eff51569429d6a95fb31ff0ee71faac4022"

// Exclusion is a classification, not a filter: every member must come back with
// an outcome, including the ones that are not leaves.
func TestResolve_EveryMemberGetsAnOutcome(t *testing.T) {
	d := dbtest.DB(t)
	people := []person{
		{id: uuid.New(), login: "alice", addr: addrA, amount: 1000},
		{id: uuid.New(), login: "bob", addr: "", amount: 2000},  // earned, no address
		{id: uuid.New(), login: "", addr: addrB, amount: 3000},  // earned, no github account
		{id: uuid.New(), login: "dave", addr: addrA, amount: 0}, // rounded to zero
		{id: uuid.New(), login: "erin", addr: addrA, amount: 0, reason: "left the programme"},
	}
	s := fixture(t, d, "aptos-testnet", people)
	members, err := Resolve(context.Background(), d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != len(people) {
		t.Fatalf("Resolve returned %d members for %d people — somebody was dropped", len(members), len(people))
	}
	got := map[Outcome]int{}
	for _, m := range members {
		if m.Outcome == "" {
			t.Fatalf("member %s has no outcome", m.UserID)
		}
		got[m.Outcome]++
	}
	for _, want := range []Outcome{OutcomePayable, OutcomeNoAddress, OutcomeNoGitHubAccount, OutcomeNoShares, OutcomeIneligible} {
		if got[want] != 1 {
			t.Errorf("outcome %s: got %d, want 1 (%v)", want, got[want], got)
		}
	}
}

// A person with no github_accounts row is reachable by design - account deletion
// keeps the users row and hard-deletes the github account - so it must classify,
// never abort the build for everyone else.
func TestResolve_MissingGitHubAccountClassifiesRatherThanErroring(t *testing.T) {
	d := dbtest.DB(t)
	s := fixture(t, d, "aptos-testnet", []person{
		{id: uuid.New(), login: "", addr: addrA, amount: 5000},
		{id: uuid.New(), login: "alice", addr: addrB, amount: 1000},
	})
	r, err := DryRun(context.Background(), d.Pool, s)
	if err != nil {
		t.Fatalf("a missing github account aborted the whole run: %v", err)
	}
	if len(r.Payable()) != 1 {
		t.Fatalf("payable = %d, want 1", len(r.Payable()))
	}
	if r.ExcludedTotalMinor.Cmp(big.NewInt(5000)) != 0 {
		t.Fatalf("undeliverable total = %s, want 5000", r.ExcludedTotalMinor)
	}
}

func TestDryRun_ReconciliationClosesAndNamesTheOwed(t *testing.T) {
	d := dbtest.DB(t)
	s := fixture(t, d, "aptos-testnet", []person{
		{id: uuid.New(), login: "alice", addr: addrA, amount: 4_000_000},
		{id: uuid.New(), login: "bob", addr: "", amount: 1_000_000},
	})
	r, err := DryRun(context.Background(), d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	if r.LeafTotalMinor.Cmp(big.NewInt(4_000_000)) != 0 {
		t.Fatalf("leaf total %s", r.LeafTotalMinor)
	}
	if r.ExcludedTotalMinor.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("excluded total %s", r.ExcludedTotalMinor)
	}
	// residue = pool - leaf total, which is what would be sweepable with no
	// timelock if the pool were funded instead.
	if r.ResidueMinor.Cmp(big.NewInt(6_000_000)) != 0 {
		t.Fatalf("residue %s, want 6000000", r.ResidueMinor)
	}
	out := r.Render()
	for _, want := range []string{"bob", "FUND EXACTLY THIS", "NO timelock", "OK", "EARNED BUT UNDELIVERABLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "DOES NOT RECONCILE") {
		t.Errorf("reconciliation failed:\n%s", out)
	}
}

// The dry run must write nothing at all - no salt, no rows.
func TestDryRun_WritesNothing(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := fixture(t, d, "aptos-testnet", []person{{id: uuid.New(), login: "alice", addr: addrA, amount: 1000}})
	if _, err := DryRun(ctx, d.Pool, s); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"payout_event_salts", "claim_leaves", "payout_event_roots"} {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE settlement_id=$1`, s.SettlementID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("the dry run wrote %d rows into %s", n, tbl)
		}
	}
}

func TestBuild_HappyPathWritesRootAndLeaves(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := fixture(t, d, "aptos-testnet", []person{
		{id: uuid.New(), login: "alice", addr: addrA, amount: 4_000_000},
		{id: uuid.New(), login: "bob", addr: addrB, amount: 1_000_000},
	})
	r, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, r.ExcludedTotalMinor})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.LeafCount != 2 {
		t.Fatalf("leaf count %d", res.LeafCount)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM claim_leaves WHERE settlement_id=$1`, s.SettlementID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("claim_leaves rows %d", n)
	}
	// The leaf must be looked up BY ADDRESS with no salt involved: that is what
	// lets the salt be destroyed later.
	var amt int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT amount_minor FROM claim_leaves WHERE settlement_id=$1 AND lower(claim_address)=lower($2)`,
		s.SettlementID, addrA).Scan(&amt); err != nil {
		t.Fatalf("address lookup: %v", err)
	}
	if amt != 4_000_000 {
		t.Fatalf("amount by address = %d", amt)
	}
}

func TestBuild_RefusesWhenInputsChanged(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	bob := uuid.New()
	s := fixture(t, d, "aptos-testnet", []person{
		{id: uuid.New(), login: "alice", addr: addrA, amount: 4_000_000},
		{id: bob, login: "bob", addr: "", amount: 1_000_000},
	})
	r, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	// bob registers an address after the report was read.
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
		VALUES ($1,'aptos-testnet',$2,'n')`, bob, addrB); err != nil {
		t.Fatal(err)
	}
	_, err = Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, r.ExcludedTotalMinor})
	if !errors.Is(err, ErrInputsChanged) {
		t.Fatalf("want ErrInputsChanged, got %v", err)
	}
}

// A login rename must land as a digest mismatch, because the rename changes the
// identity hash that goes into a permanent root.
func TestBuild_ARenameIsADigestMismatch(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	alice := uuid.New()
	s := fixture(t, d, "aptos-testnet", []person{{id: alice, login: "alice", addr: addrA, amount: 1000}})
	r, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE github_accounts SET login='alice-renamed' WHERE user_id=$1`, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, r.ExcludedTotalMinor}); !errors.Is(err, ErrInputsChanged) {
		t.Fatalf("a rename did not produce a digest mismatch: %v", err)
	}
}

func TestBuild_RequiresTheExcludedTotalRestated(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := fixture(t, d, "aptos-testnet", []person{
		{id: uuid.New(), login: "alice", addr: addrA, amount: 4_000_000},
		{id: uuid.New(), login: "bob", addr: "", amount: 1_000_000},
	})
	r, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*big.Int{nil, big.NewInt(0), big.NewInt(999_999), big.NewInt(1_000_001)} {
		if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, bad}); !errors.Is(err, ErrNotAcknowledged) {
			t.Errorf("acknowledgement %v: want ErrNotAcknowledged, got %v", bad, err)
		}
	}
}

func TestBuild_RefusesASecondRoot(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := fixture(t, d, "aptos-testnet", []person{{id: uuid.New(), login: "alice", addr: addrA, amount: 1000}})
	r, _ := DryRun(ctx, d.Pool, s)
	if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, r.ExcludedTotalMinor}); err != nil {
		t.Fatal(err)
	}
	r2, _ := DryRun(ctx, d.Pool, s)
	if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r2.InputDigest, r2.ExcludedTotalMinor}); !errors.Is(err, ErrAlreadyBuilt) {
		t.Fatalf("a second build was allowed: %v", err)
	}
}

// An unrecognised pool must error, not default. The pool is hashed into every
// leaf, so a default would build a consistent tree for the wrong pool.
func TestBuild_UnknownPoolIsRefusedNotDefaulted(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := fixture(t, d, "aptos-testnet", []person{{id: uuid.New(), login: "alice", addr: addrA, amount: 1000}})
	s.Pool = "contrbutor" // a typo
	r, err := DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, d.Pool, saltKey(t), s, Acknowledgement{r.InputDigest, r.ExcludedTotalMinor}); !errors.Is(err, ErrUnknownPool) {
		t.Fatalf("a typo'd pool built a tree: %v", err)
	}
}

func TestInputDigest_ChangesWhenAnyInputMoves(t *testing.T) {
	id := uuid.New()
	base := Settlement{SettlementID: uuid.New(), ChainID: "aptos-testnet", Pool: "contributor"}
	m := []Member{{UserID: id, GitHubLogin: "alice", ClaimAddress: addrA, AmountMinor: big.NewInt(10), Outcome: OutcomePayable}}
	d0 := InputDigest(base, m)

	variants := map[string][]Member{
		"login":   {{UserID: id, GitHubLogin: "alice2", ClaimAddress: addrA, AmountMinor: big.NewInt(10)}},
		"address": {{UserID: id, GitHubLogin: "alice", ClaimAddress: addrB, AmountMinor: big.NewInt(10)}},
		"amount":  {{UserID: id, GitHubLogin: "alice", ClaimAddress: addrA, AmountMinor: big.NewInt(11)}},
		"user":    {{UserID: uuid.New(), GitHubLogin: "alice", ClaimAddress: addrA, AmountMinor: big.NewInt(10)}},
	}
	for name, v := range variants {
		if InputDigest(base, v) == d0 {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}

	// The classification is derived, so an excluded member must still be in the
	// digest: a change that moves somebody between classifications has to show.
	withExcluded := append([]Member{}, m...)
	withExcluded = append(withExcluded, Member{UserID: uuid.New(), GitHubLogin: "bob", ClaimAddress: "", AmountMinor: big.NewInt(5), Outcome: OutcomeNoAddress})
	if InputDigest(base, withExcluded) == d0 {
		t.Error("an excluded member is not in the digest; a move between classifications could cancel out")
	}
}

// Length prefixes: two members whose fields differ only in where one ends must
// not collide.
func TestInputDigest_FieldBoundariesAreRecoverable(t *testing.T) {
	id := uuid.New()
	base := Settlement{SettlementID: uuid.New(), ChainID: "c", Pool: "contributor"}
	a := []Member{{UserID: id, GitHubLogin: "ab", ClaimAddress: "cd", AmountMinor: big.NewInt(1)}}
	b := []Member{{UserID: id, GitHubLogin: "abc", ClaimAddress: "d", AmountMinor: big.NewInt(1)}}
	if InputDigest(base, a) == InputDigest(base, b) {
		t.Fatal("field boundaries collide in the digest")
	}
}
