package grainhack

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// One rail per event pool, in both directions, for the GrainHack statement
// against each of the other two rails - in Go (a readable refusal before any
// work) and in the database (the half that holds for every writer).

func (f *fx) openKeeperHubRun(pool string) error {
	_, err := f.d.Pool.Exec(context.Background(), `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id, state)
		VALUES ($1, $2, 'base-sepolia', 84532, 10000000, $3, 'planned')`, f.hid, pool, f.payoutRun)
	return err
}

func (f *fx) persistAptosSettlement() error {
	return settlement.Persist(context.Background(), f.d.Pool, &settlement.Result{
		HackathonID:    &f.hid,
		Pool:           "contributor",
		ChainID:        "aptos-testnet",
		PoolMinor:      big.NewInt(1_000_000),
		AssetDecimals:  settlement.AssetDecimals,
		TotalEffective: big.NewRat(1, 1),
		Lines: []settlement.Line{{
			UserID: f.people[0].userID, RawWeight: big.NewRat(1, 1), Multiplier: big.NewRat(1, 1),
			EffectiveWeight: big.NewRat(1, 1), AmountMinor: big.NewInt(1_000_000),
		}},
	})
}

// rawStatement inserts a statement row directly, as a writer that skips the
// Go checks would - the trigger alone must refuse it.
func (f *fx) rawStatement() error {
	_, err := f.d.Pool.Exec(context.Background(), `
		INSERT INTO grainhack_results_statements
		  (id, hackathon_id, pool, computation_id, currency, network, pool_minor,
		   canonical_json, signature, signing_public_key, issued_by, issued_at)
		VALUES ($1, $2, 'contributor', $3, 'USDC', 'solana-devnet', 1, 'x', 'x', 'x', $4, now())`,
		uuid.New(), f.hid, f.payoutRun, f.admin)
	return err
}

func TestRail_KeeperHubRunBlocksAStatement(t *testing.T) {
	f := fixture(t)
	if err := f.openKeeperHubRun("contributor"); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Issue(context.Background(), f.req())
	var other *OtherRailError
	if !errors.As(err, &other) || other.Rail != "keeperhub" || !errors.Is(err, ErrPaidOnOtherRail) {
		t.Fatalf("err = %v, want OtherRailError on keeperhub", err)
	}
	if code := pgCode(f.rawStatement()); code != SQLStateStatementRefused {
		t.Fatalf("raw statement insert: code %q, want %s", code, SQLStateStatementRefused)
	}
	if n := f.count(`SELECT count(*) FROM grainhack_results_statements WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("%d statement(s) written", n)
	}
}

func TestRail_AptosSettlementBlocksAStatement(t *testing.T) {
	f := fixture(t)
	if err := f.persistAptosSettlement(); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Issue(context.Background(), f.req())
	var other *OtherRailError
	if !errors.As(err, &other) || other.Rail != "aptos" {
		t.Fatalf("err = %v, want OtherRailError on aptos", err)
	}
	if code := pgCode(f.rawStatement()); code != SQLStateStatementRefused {
		t.Fatalf("raw statement insert: code %q, want %s", code, SQLStateStatementRefused)
	}
}

func TestRail_StatementBlocksAKeeperHubRun(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Issue(context.Background(), f.req()); err != nil {
		t.Fatal(err)
	}
	if code := pgCode(f.openKeeperHubRun("contributor")); code != SQLStateKeeperHubRefused {
		t.Fatalf("KeeperHub run after a statement: code %q, want %s", code, SQLStateKeeperHubRefused)
	}
	if n := f.count(`SELECT count(*) FROM keeperhub_payout_runs WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("%d KeeperHub run(s) written", n)
	}
}

func TestRail_StatementBlocksAnAptosSettlement(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Issue(context.Background(), f.req()); err != nil {
		t.Fatal(err)
	}
	err := f.persistAptosSettlement()
	if !errors.Is(err, settlement.ErrEventPaidOnGrainHack) {
		t.Fatalf("settlement after a statement: err = %v, want ErrEventPaidOnGrainHack", err)
	}
	var gx *settlement.GrainHackExclusionError
	if !errors.As(err, &gx) || gx.HackathonID != f.hid.String() {
		t.Fatalf("err = %#v, want a GrainHackExclusionError naming the event", err)
	}
	if n := f.count(`SELECT count(*) FROM settlements WHERE hackathon_id = $1`, f.hid); n != 0 {
		t.Fatalf("%d settlement(s) written", n)
	}
}

// The pool is part of the key, as with KH001/KH002.
func TestRail_OnlyTheSamePoolIsBlocked(t *testing.T) {
	f := fixture(t)
	if err := f.openKeeperHubRun("maintainer"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Issue(context.Background(), f.req()); err != nil {
		t.Fatalf("a maintainer-pool KeeperHub run blocked the contributor statement: %v", err)
	}
}

// Founding settlements have no event and are untouched by the new trigger.
func TestRail_FoundingSettlementIsUnaffected(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Issue(context.Background(), f.req()); err != nil {
		t.Fatal(err)
	}
	res := &settlement.Result{
		Pool: "contributor", ChainID: "aptos-testnet", PoolMinor: big.NewInt(1_000_000),
		AssetDecimals: settlement.AssetDecimals, TotalEffective: big.NewRat(1, 1),
		Lines: []settlement.Line{{
			UserID: f.people[0].userID, RawWeight: big.NewRat(1, 1), Multiplier: big.NewRat(1, 1),
			EffectiveWeight: big.NewRat(1, 1), AmountMinor: big.NewInt(1_000_000),
		}},
	}
	if err := settlement.Persist(context.Background(), f.d.Pool, res); err != nil {
		t.Fatalf("founding settlement refused: %v", err)
	}
	f.d.Pool.Exec(context.Background(), `DELETE FROM settlement_lines WHERE settlement_id = $1`, res.SettlementID)
	f.d.Pool.Exec(context.Background(), `DELETE FROM settlements WHERE id = $1`, res.SettlementID)
}

// The rail check is serialised per event pool: a KeeperHub run started while a
// statement's transaction is still open waits for it, then sees it and is
// refused - rather than both reading the other's table as empty and both
// committing.
func TestRail_ConcurrentWritersAreSerialised(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()

	tx, err := f.d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	sid := uuid.New()
	p := f.people[0]
	if _, err := tx.Exec(ctx, `
		INSERT INTO grainhack_results_statements
		  (id, hackathon_id, pool, computation_id, currency, network, pool_minor,
		   canonical_json, signature, signing_public_key, issued_by, issued_at)
		VALUES ($1, $2, 'contributor', $3, 'USDC', 'solana-devnet', 1, 'x', 'x', 'x', $4, now())`,
		sid, f.hid, f.payoutRun, f.admin); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO grainhack_results_statement_lines (statement_id, github_user_id, user_id, login, amount_minor, status)
		VALUES ($1, $2, $3, $4, 1, 'payable')`, sid, p.githubUserID, p.userID, p.login); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- f.openKeeperHubRun("contributor") }()
	select {
	case err := <-done:
		t.Fatalf("the KeeperHub insert did not wait for the open statement transaction (err = %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if code := pgCode(err); code != SQLStateKeeperHubRefused {
			t.Fatalf("after the statement committed: code %q (%v), want %s", code, err, SQLStateKeeperHubRefused)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the KeeperHub insert never finished")
	}
}
