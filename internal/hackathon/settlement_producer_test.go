package hackathon

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

func newSettleFixture(t *testing.T, d *db.DB, contributorPool string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var hid, pid, owner uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		owner, "acme/repo-"+uuid.NewString()[:8]).Scan(&pid); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO hackathons (name, contributor_prize_pool) VALUES ($1, $2::numeric) RETURNING id`,
		"t-"+uuid.NewString()[:8], contributorPool).Scan(&hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM hackathons WHERE id=$1`, hid)
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1`, pid)
	})
	return hid, pid
}

func addVerdict(t *testing.T, d *db.DB, hid, pid uuid.UUID, pr int, bucket string, units *int, mult *string) uuid.UUID {
	t.Helper()
	var uid uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric)`,
		hid, pid, pr, uid, "gh-"+uuid.NewString()[:6], bucket, units, mult); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	return uid
}

func addVerdictFor(t *testing.T, d *db.DB, hid, pid, uid uuid.UUID, pr int, bucket string, units int, mult string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric)`,
		hid, pid, pr, uid, "gh-x", bucket, units, mult); err != nil {
		t.Fatalf("verdict: %v", err)
	}
}

func ptrI(v int) *int       { return &v }
func ptrS(v string) *string { return &v }

// The whole pool is allocated, exactly. This is the invariant a published root
// cannot survive being wrong about.
func TestSettlementFor_AllocatesTheWholePool(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "1000")
	addVerdict(t, d, hid, pid, 1, "exceptional", ptrI(3), ptrS("1"))
	addVerdict(t, d, hid, pid, 2, "accepted", ptrI(1), ptrS("1"))

	res, err := SettlementFor(context.Background(), d.Pool, hid, "contributor", "aptos-testnet")
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	want := big.NewInt(1_000_000_000) // 1000 USDC in minor units
	if got := res.TotalAllocatedMinor(); got.Cmp(want) != 0 {
		t.Errorf("allocated %s, want %s", got, want)
	}
	if res.Pool != "contributor" || res.ChainID != "aptos-testnet" {
		t.Errorf("pool=%q chain=%q", res.Pool, res.ChainID)
	}
	if res.HackathonID == nil || *res.HackathonID != hid {
		t.Error("settlement is not attributed to its hackathon")
	}
}

// A rejected submission earns nothing and must not consume weight, or every
// other contributor is quietly paid less.
func TestSettlementFor_RejectedTakesNoShare(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "100")
	addVerdict(t, d, hid, pid, 1, "accepted", ptrI(1), ptrS("1"))
	addVerdict(t, d, hid, pid, 2, "rejected", ptrI(5), ptrS("1"))

	res, err := SettlementFor(context.Background(), d.Pool, hid, "contributor", "c")
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	if n := len(res.Lines); n != 1 {
		t.Fatalf("lines = %d, want 1 - a rejected verdict became a line", n)
	}
	if got := res.Lines[0].AmountMinor; got.Cmp(big.NewInt(100_000_000)) != 0 {
		t.Errorf("the single accepted line got %s, want the whole pool", got)
	}
}

// One person, several merged PRs, one line. Two lines for one user would break
// (settlement_id, user_id) uniqueness and put two leaves in the tree for one
// identity.
func TestSettlementFor_SumsMultiplePRsIntoOneLine(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "90")
	uid := addVerdict(t, d, hid, pid, 1, "accepted", ptrI(1), ptrS("1"))
	addVerdictFor(t, d, hid, pid, uid, 2, "accepted", 2, "1")

	res, err := SettlementFor(context.Background(), d.Pool, hid, "contributor", "c")
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	if n := len(res.Lines); n != 1 {
		t.Fatalf("lines = %d, want 1", n)
	}
	if got := res.Lines[0].RawWeight.RatString(); got != "3" {
		t.Errorf("raw weight = %s, want 3 (1+2)", got)
	}
}

// The curve multiplier is applied exactly, not through float64. 0.8 of 3 units
// is 12/5, and a binary float cannot represent 0.8.
func TestSettlementFor_CurveMultiplierIsExact(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "10")
	addVerdict(t, d, hid, pid, 1, "accepted", ptrI(3), ptrS("0.8"))

	res, err := SettlementFor(context.Background(), d.Pool, hid, "contributor", "c")
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	if got := res.Lines[0].EffectiveWeight.RatString(); got != "12/5" {
		t.Errorf("effective weight = %s, want 12/5 exactly", got)
	}
}

// An event where everything was rejected settles to nothing and says so, rather
// than erroring or dividing by zero.
func TestSettlementFor_NothingToSettleIsAnOrdinaryOutcome(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "100")
	addVerdict(t, d, hid, pid, 1, "rejected", ptrI(0), ptrS("1"))

	_, err := SettlementFor(context.Background(), d.Pool, hid, "contributor", "c")
	if !errors.Is(err, ErrNothingToSettle) {
		t.Fatalf("err = %v, want ErrNothingToSettle", err)
	}
}

// The maintainer pool is not derived from verdicts, and saying so is better than
// returning contributor rows under a maintainer label.
func TestSettlementFor_MaintainerPoolRefuses(t *testing.T) {
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "100")
	addVerdict(t, d, hid, pid, 1, "accepted", ptrI(1), ptrS("1"))

	if _, err := SettlementFor(context.Background(), d.Pool, hid, "maintainer", "c"); err == nil {
		t.Fatal("the maintainer pool settled from verdicts, which is not where its money comes from")
	}
}

// Persisting twice for the same event and pool is refused by the database, which
// is what stops a second Merkle root existing for money already published.
func TestSettlementFor_PersistIsIdempotentPerEventAndPool(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	hid, pid := newSettleFixture(t, d, "100")
	addVerdict(t, d, hid, pid, 1, "accepted", ptrI(1), ptrS("1"))

	res, err := SettlementFor(ctx, d.Pool, hid, "contributor", "c")
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	if err := settlement.Persist(ctx, d.Pool, res); err != nil {
		t.Fatalf("first Persist: %v", err)
	}
	again, err := SettlementFor(ctx, d.Pool, hid, "contributor", "c")
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if err := settlement.Persist(ctx, d.Pool, again); err == nil {
		t.Fatal("a second settlement for the same event and pool was accepted")
	}
}
