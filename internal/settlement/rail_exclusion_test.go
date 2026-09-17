package settlement_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// A settled-looking event: a hackathon and its computed payout run.
func eventFixture(t *testing.T, d *db.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var hid, prun uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name, contributor_prize_pool) VALUES ($1, 1) RETURNING id`,
		"rail-x-"+uuid.NewString()[:8]).Scan(&hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value)
		VALUES ($1, 1, 1, 1) RETURNING id`, hid).Scan(&prun); err != nil {
		t.Fatalf("payout run: %v", err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM settlements WHERE hackathon_id = $1`, hid)
		d.Pool.Exec(context.Background(), `DELETE FROM hackathons WHERE id = $1`, hid)
	})
	return hid, prun
}

// openKeeperHubRun records that the event is being paid on the KeeperHub rail.
// A PLANNED run is enough: a planned run can still dispatch, so waiting for it
// to pay would leave a window.
func openKeeperHubRun(t *testing.T, d *db.DB, hid, prun uuid.UUID, pool string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id, state)
		VALUES ($1, $2, 'base-sepolia', 84532, 1000000, $3, 'planned')`, hid, pool, prun); err != nil {
		t.Fatalf("keeperhub run: %v", err)
	}
}

// oneLine is a minimal, reconciling settlement for one person.
func oneLine(t *testing.T, d *db.DB, hid *uuid.UUID) *settlement.Result {
	t.Helper()
	var uid uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM settlement_lines WHERE user_id = $1`, uid)
		d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, uid)
	})
	return &settlement.Result{
		HackathonID:    hid,
		Pool:           "contributor",
		ChainID:        "aptos-testnet",
		PoolMinor:      big.NewInt(1_000_000),
		AssetDecimals:  settlement.AssetDecimals,
		TotalEffective: big.NewRat(1, 1),
		Lines: []settlement.Line{{
			UserID: uid, RawWeight: big.NewRat(1, 1), Multiplier: big.NewRat(1, 1),
			EffectiveWeight: big.NewRat(1, 1), AmountMinor: big.NewInt(1_000_000),
		}},
	}
}

func settlementsFor(t *testing.T, d *db.DB, hid uuid.UUID) int {
	t.Helper()
	var n int
	d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM settlements WHERE hackathon_id = $1`, hid).Scan(&n)
	return n
}

// THE MIRROR. An event already being paid on KeeperHub must not also be
// settled on Aptos. Before this, only the other direction was refused.
func TestPersist_RefusesAnEventBeingPaidOnKeeperHub(t *testing.T) {
	d := dbtest.DB(t)
	hid, prun := eventFixture(t, d)
	openKeeperHubRun(t, d, hid, prun, "contributor")

	err := settlement.Persist(context.Background(), d.Pool, oneLine(t, d, &hid))
	if err == nil {
		t.Fatalf("a settlement was recorded for hackathon %s, which already has a KeeperHub run - "+
			"KeeperHub-then-Aptos is not blocked, so both rails could pay the same people", hid)
	}
	if !errors.Is(err, settlement.ErrEventPaidOnKeeperHub) {
		t.Fatalf("err = %v, want ErrEventPaidOnKeeperHub", err)
	}
	var rx *settlement.RailExclusionError
	if !errors.As(err, &rx) || rx.HackathonID != hid.String() || rx.Pool != "contributor" {
		t.Fatalf("err = %#v, want a RailExclusionError naming the event and pool", err)
	}
	// The operator reads a policy sentence, never the database exception.
	msg := err.Error()
	for _, raw := range []string{"SQLSTATE", "ERROR:", "KH001", "insert settlement"} {
		if strings.Contains(msg, raw) {
			t.Errorf("the refusal leaks %q to the operator: %s", raw, msg)
		}
	}
	if !strings.Contains(msg, "paid on one rail only") || !strings.Contains(msg, "Nothing was written") {
		t.Errorf("the refusal does not explain itself: %s", msg)
	}
	if n := settlementsFor(t, d, hid); n != 0 {
		t.Errorf("%d settlements row(s) written despite the refusal", n)
	}
}

// Symmetric with #553: the pool is part of the key. A KeeperHub run on the
// maintainer pool does not block the contributor pool.
func TestPersist_OnlyTheSamePoolIsRefused(t *testing.T) {
	d := dbtest.DB(t)
	hid, prun := eventFixture(t, d)
	openKeeperHubRun(t, d, hid, prun, "maintainer")

	if err := settlement.Persist(context.Background(), d.Pool, oneLine(t, d, &hid)); err != nil {
		t.Fatalf("contributor pool refused because of a maintainer-pool run: %v", err)
	}
}

func TestPersist_AnEventWithNoKeeperHubRunIsAccepted(t *testing.T) {
	d := dbtest.DB(t)
	hid, _ := eventFixture(t, d)
	if err := settlement.Persist(context.Background(), d.Pool, oneLine(t, d, &hid)); err != nil {
		t.Fatalf("an event with no KeeperHub run was refused: %v", err)
	}
	if n := settlementsFor(t, d, hid); n != 1 {
		t.Errorf("settlements for the event = %d, want 1", n)
	}
}

// Founding settlements carry no hackathon and must be entirely unaffected -
// including when KeeperHub runs exist for other events.
func TestPersist_AFoundingSettlementIsAccepted(t *testing.T) {
	d := dbtest.DB(t)
	hid, prun := eventFixture(t, d)
	openKeeperHubRun(t, d, hid, prun, "contributor")

	res := oneLine(t, d, nil)
	if err := settlement.Persist(context.Background(), d.Pool, res); err != nil {
		t.Fatalf("a founding settlement (NULL hackathon_id) was refused: %v", err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM settlement_lines WHERE settlement_id = $1`, res.SettlementID)
		d.Pool.Exec(context.Background(), `DELETE FROM settlements WHERE id = $1`, res.SettlementID)
	})
	var hackathon *uuid.UUID
	d.Pool.QueryRow(context.Background(), `SELECT hackathon_id FROM settlements WHERE id = $1`, res.SettlementID).Scan(&hackathon)
	if hackathon != nil {
		t.Errorf("founding settlement stored with hackathon_id %s", hackathon)
	}
}
