package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
)

const (
	// Distinct from internal/payout's fixture addresses, on purpose.
	//
	// Migration 086 makes one live address belong to one account, so two packages
	// sharing an address constant collide on that index if they ever run at the
	// same time against one database - and the failure surfaces as "no payable
	// members", three layers from the insert that lost.
	//
	// This removes THAT collision and nothing else. It does not make the suite
	// safe to run concurrently: packages sharing one Postgres also race on
	// schema_migrations and on broad table state, which is why CI passes -p 1 and
	// says so at length in ci.yml. Run it the way CI does.
	addrAStem = "aaaa0001"
	addrBStem = "aaaa0002"
)

// Unique per PROCESS, not fixed.
//
// addrA and addrB were constants, which made every test using them pass exactly
// once against a given database: migration 086 makes one live address belong to
// one account, this database is never truncated, and a run that fails before its
// cleanup registers leaves a row that collides on every future run for ever.
// That is precisely what happened - one orphan from one failed run made four
// tests fail with a duplicate-key error unrelated to what they test.
//
// Same defect as the reconciler fixture's process-local counter: a fixture value
// that repeats is a fixture that works once. The tests do not care what the
// address is, only that it is theirs.
var (
	addrRun = strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	addrA   = "0x" + addrAStem + addrRun + strings.Repeat("0", 64-len(addrAStem)-len(addrRun))
	addrB   = "0x" + addrBStem + addrRun + strings.Repeat("0", 64-len(addrBStem)-len(addrRun))
)

// addrFor derives a distinct canonical address per user.
//
// Fixtures used to share two constants, which was fine until migration 086 made
// one live address belong to one account: the second test registering the same
// address hit the unique index, the fixture ignored the error, and the failure
// surfaced three layers away as "no payable members". Unique per user, and every
// fixture insert now checks its error.
func addrFor(id uuid.UUID) string {
	h := strings.ReplaceAll(id.String(), "-", "")
	return "0x" + strings.Repeat("0", 64-len(h)) + h
}

func mustExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture: %v\n  sql: %.90s", err, sql)
	}
}

func saltKeyB64() string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// appFor mounts the claims routes with the user id already in Locals, so these
// exercise the handler rather than the auth middleware.
func appFor(uid uuid.UUID, d *db.DB) *fiber.App {
	app := fiber.New()
	h := NewPayoutClaimsHandler(d)
	inject := func(c *fiber.Ctx) error { c.Locals(auth.LocalUserID, uid.String()); return c.Next() }
	app.Get("/me/claims", inject, h.GetClaims)
	app.Get("/me/claims/:settlement_id", inject, h.GetClaim)
	return app
}

func getJSON(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
	t.Helper()
	res, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return res.StatusCode, out
}

// seedClaim builds a real settlement, tree and publication for one user.
func seedClaim(t *testing.T, d *db.DB, uid uuid.UUID, login, addr string, amount int64) uuid.UUID {

	t.Helper()
	ctx := context.Background()
	var sid uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, pool_minor, asset_decimals)
		VALUES (1,1,1,$1,6) RETURNING id`, amount*4).Scan(&sid); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	// Verified, because an unverified contributor is now HELD rather than paid
	// (#507) and would have no leaf - so every claims test would be asserting
	// against an empty list for a reason that has nothing to do with claims.
	mustExec(t, d, `INSERT INTO users (id, role, kyc_status) VALUES ($1,'contributor','verified')`, uid)
	mustExec(t, d, `INSERT INTO github_accounts (id,user_id,github_user_id,login,access_token,created_at,updated_at)
		VALUES (gen_random_uuid(),$1,$2,$3,'\x00',now(),now())`, uid, int64(uuid.New().ID()), login)
	mustExec(t, d, `INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
		VALUES ($1,'aptos-testnet',$2,'n')`, uid, addr)

	s, err := payout.LoadSettlement(ctx, d.Pool, sid, "aptos-testnet")
	if err != nil {
		// LoadSettlement needs lines; insert one directly.
		d.Pool.Exec(ctx, `INSERT INTO settlement_lines (id,settlement_id,user_id,raw_weight,multiplier,effective_weight,usdc_amount,amount_minor)
			VALUES (gen_random_uuid(),$1,$2,1,1,1,0,$3)`, sid, uid, amount)
		s, err = payout.LoadSettlement(ctx, d.Pool, sid, "aptos-testnet")
		if err != nil {
			t.Fatalf("load settlement: %v", err)
		}
	}
	r, err := payout.DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := payout.Build(ctx, d.Pool, saltKeyB64(), s,
		payout.Acknowledgement{InputDigest: r.InputDigest, ExcludedTotalMinor: r.ExcludedTotalMinor}); err != nil {
		t.Fatalf("build: %v", err)
	}
	d.Pool.Exec(ctx, `UPDATE payout_event_roots SET published_tx='0xtx', escrow_address='0xesc', published_at=now()
		WHERE settlement_id=$1`, sid)

	t.Cleanup(func() {
		d.Pool.Exec(ctx, `DELETE FROM claim_leaves WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_roots WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_salts WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM settlement_lines WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM contributor_addresses WHERE user_id=$1`, uid)
		d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id=$1`, uid)
		d.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return sid
}

// Everything a client would otherwise hardcode must be on the row.
func TestClaims_ServesEveryValueAClientWouldHardcode(t *testing.T) {
	d := dbtest.DB(t)
	uid := uuid.New()
	seedClaim(t, d, uid, "alice", addrA, 250_000)

	code, body := getJSON(t, appFor(uid, d), "/me/claims")
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	list, _ := body["claims"].([]any)
	if len(list) != 1 {
		t.Fatalf("claims = %d", len(list))
	}
	cl := list[0].(map[string]any)

	for _, k := range []string{"contract_address", "explorer_url_template", "network",
		"escrow_address", "claim_address_verified_at", "identity_hash", "proof", "root"} {
		v, ok := cl[k]
		if !ok {
			t.Errorf("missing %q — a client would have to hardcode or guess it", k)
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			t.Errorf("%q is an empty string, which a client will use as though it were real", k)
		}
	}
	if got := cl["contract_address"]; got == "" || got == nil {
		t.Error("contract_address is the whole point of #519")
	}
	// The network is a label the SDK resolves, never a URL we must keep alive.
	if n, _ := cl["network"].(string); n != "testnet" {
		t.Errorf("network = %q, want the label \"testnet\"", n)
	}
	// The symbol used to be a server-side literal beside decimals read from config.
	// Asserting "USDC" here would pass whether the symbol comes from the row or
	// from a literal, because the seeded value is USDC. So change the row and
	// require the response to follow.
	asset, _ := cl["asset"].(map[string]any)
	if asset["symbol"] != "USDC" {
		t.Errorf("asset.symbol = %v", asset["symbol"])
	}
	ctx := context.Background()
	d.Pool.Exec(ctx, `UPDATE chain_configs SET asset = jsonb_set(asset,'{symbol}','"ZZZ"') WHERE chain_id='aptos-testnet'`)
	t.Cleanup(func() {
		d.Pool.Exec(ctx, `UPDATE chain_configs SET asset = jsonb_set(asset,'{symbol}','"USDC"') WHERE chain_id='aptos-testnet'`)
	})
	_, body2 := getJSON(t, appFor(uid, d), "/me/claims")
	cl2 := body2["claims"].([]any)[0].(map[string]any)
	asset2, _ := cl2["asset"].(map[string]any)
	if asset2["symbol"] != "ZZZ" {
		t.Errorf("asset.symbol = %v after changing the row; it is a literal, not served config", asset2["symbol"])
	}
}

// The registration date of the FROZEN address is otherwise unreachable:
// GET /me/payout-address filters on superseded_at IS NULL.
func TestClaims_CarriesTheFrozenAddressRegistrationDate(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid := uuid.New()
	seedClaim(t, d, uid, "alice", addrA, 250_000)

	// Supersede it, as registering a new address does.
	replacement := addrB
	d.Pool.Exec(ctx, `UPDATE contributor_addresses SET superseded_at=now() WHERE user_id=$1`, uid)
	d.Pool.Exec(ctx, `INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
		VALUES ($1,'aptos-testnet',$2,'n2')`, uid, replacement)

	_, body := getJSON(t, appFor(uid, d), "/me/claims")
	cl := body["claims"].([]any)[0].(map[string]any)

	if cl["claim_address_verified_at"] == nil || cl["claim_address_verified_at"] == "" {
		t.Fatal("no registration date for the frozen address; \"the address you registered on 3 July\" " +
			"cannot be written against this API")
	}
	if cl["address_status"] != "superseded" {
		t.Errorf("address_status = %v, want superseded", cl["address_status"])
	}
	if cl["current_address"] != replacement {
		t.Errorf("current_address = %v, want the live one", cl["current_address"])
	}
}

// Three states, not two. The third is not the second with a null beside it.
func TestClaims_AddressStatusHasThreeStates(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid := uuid.New()
	seedClaim(t, d, uid, "alice", addrA, 250_000)
	app := appFor(uid, d)

	_, body := getJSON(t, app, "/me/claims")
	cl := body["claims"].([]any)[0].(map[string]any)
	if cl["address_status"] != "current" {
		t.Fatalf("state 1: got %v", cl["address_status"])
	}

	// Superseded WITHOUT a replacement - an admin action or a future removal.
	d.Pool.Exec(ctx, `UPDATE contributor_addresses SET superseded_at=now() WHERE user_id=$1`, uid)
	_, body = getJSON(t, app, "/me/claims")
	cl = body["claims"].([]any)[0].(map[string]any)
	if cl["address_status"] != "no_live_address" {
		t.Fatalf("state 3: got %v, want no_live_address — this person must claim from a wallet "+
			"they no longer have registered AND has nothing registered for future payouts",
			cl["address_status"])
	}
	if cl["current_address"] != nil {
		t.Errorf("current_address should be null in this state, got %v", cl["current_address"])
	}
}

// Returning the first of several and dropping the rest answers a question about
// money with an arbitrary choice.
//
// Two REAL leaves, both in the published tree, both paying addresses registered
// to one account. An earlier version of this test injected a fabricated leaf row
// instead, and ClaimFor correctly refused to serve a proof from leaves that no
// longer reproduce the published root - the drift check doing its job, and a
// reminder that a fixture which bypasses an invariant tests the invariant, not
// the thing you meant.
func TestGetClaim_RefusesRatherThanReturningTheFirstOfSeveral(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid, other := uuid.New(), uuid.New()

	var sid uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, pool_minor, asset_decimals)
		VALUES (1,1,1,1000000,6) RETURNING id`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	for i, u := range []uuid.UUID{uid, other} {
		addr := addrA
		if i == 1 {
			addr = addrB
		}
		// Verified for the same reason seedClaim is: unverified is now a hold,
		// and a held member has no leaf, so this test's two-matches setup
		// would never produce even one.
		d.Pool.Exec(ctx, `INSERT INTO users (id, role, kyc_status) VALUES ($1,'contributor','verified')`, u)
		d.Pool.Exec(ctx, `INSERT INTO github_accounts (id,user_id,github_user_id,login,access_token,created_at,updated_at)
			VALUES (gen_random_uuid(),$1,$2,$3,'\x00',now(),now())`, u, int64(uuid.New().ID()), fmt.Sprintf("u%d", i))
		d.Pool.Exec(ctx, `INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
			VALUES ($1,'aptos-testnet',$2,'n')`, u, addr)
		d.Pool.Exec(ctx, `INSERT INTO settlement_lines (id,settlement_id,user_id,raw_weight,multiplier,effective_weight,usdc_amount,amount_minor)
			VALUES (gen_random_uuid(),$1,$2,1,1,1,0,$3)`, sid, u, 250000)
	}
	t.Cleanup(func() {
		d.Pool.Exec(ctx, `DELETE FROM claim_leaves WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_roots WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM payout_event_salts WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM settlement_lines WHERE settlement_id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE id=$1`, sid)
		for _, u := range []uuid.UUID{uid, other} {
			d.Pool.Exec(ctx, `DELETE FROM contributor_addresses WHERE user_id=$1`, u)
			d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id=$1`, u)
			d.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, u)
		}
	})

	s, err := payout.LoadSettlement(ctx, d.Pool, sid, "aptos-testnet")
	if err != nil {
		t.Fatal(err)
	}
	r, err := payout.DryRun(ctx, d.Pool, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payout.Build(ctx, d.Pool, saltKeyB64(), s,
		payout.Acknowledgement{InputDigest: r.InputDigest, ExcludedTotalMinor: r.ExcludedTotalMinor}); err != nil {
		t.Fatal(err)
	}
	d.Pool.Exec(ctx, `UPDATE payout_event_roots SET published_tx='0xtx', escrow_address='0xesc', published_at=now()
		WHERE settlement_id=$1`, sid)

	// The realistic route to two matches: the live-address index is unique per
	// (user, chain), not per address, so two accounts can register one address.
	// Recorded here as superseded so it does not collide with uid's live row.
	d.Pool.Exec(ctx, `INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce, superseded_at)
		VALUES ($1,'aptos-testnet',$2,'n2', now())`, uid, addrB)

	code, body := getJSON(t, appFor(uid, d), "/me/claims/"+sid.String())
	if code == 200 {
		t.Fatalf("returned one of two matches instead of refusing: %v", body)
	}
	if body["error"] != "multiple_claims_for_settlement" {
		t.Fatalf("error = %v, want multiple_claims_for_settlement (body: %v)", body["error"], body)
	}
	if n, _ := body["count"].(float64); int(n) != 2 {
		t.Errorf("count = %v, want 2", body["count"])
	}
}

var _ = fmt.Sprintf
var _ = big.NewInt
