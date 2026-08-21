package sponsor

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The floor is derived from a measured transaction, not rounded. If somebody
// changes a constant, this says what the number is supposed to mean.
func TestThresholds_DerivedFromTheMeasuredCost(t *testing.T) {
	if GasOctasPerClaim != 1_492_000 {
		t.Fatalf("per-claim cost = %d; the milestone measured 14,920 units x 100 octas", GasOctasPerClaim)
	}
	// One full founding settlement, three times over.
	if HardFloorOctas != 38*1_492_000*3 {
		t.Fatalf("hard floor = %d, want 38 x 1,492,000 x 3", HardFloorOctas)
	}
	if HardFloorOctas != 170_088_000 {
		t.Fatalf("hard floor = %d octas (%.4f APT), want 170,088,000 (1.7009 APT)",
			HardFloorOctas, float64(HardFloorOctas)/1e8)
	}
}

// The alarm sits ABOVE the floor, so the warning arrives while there is still
// room to act rather than at the moment sponsorship stops.
func TestThresholds_AlarmIsNeverBelowTheFloor(t *testing.T) {
	for _, unclaimed := range []int{0, 1, 10, 38, 200} {
		if got := AlarmThresholdOctas(unclaimed); got < HardFloorOctas {
			t.Errorf("alarm for %d unclaimed = %d, below the floor %d", unclaimed, got, HardFloorOctas)
		}
	}
	// And it scales past the floor for a larger settlement, because a fixed
	// number is only correct for 38 people.
	if AlarmThresholdOctas(200) <= HardFloorOctas {
		t.Error("the alarm does not scale above the floor for a larger settlement")
	}
}

func TestCheckBalance_RefusesBeforeTheFloorIsCrossed(t *testing.T) {
	g := NewGuard(nil)
	// Comfortably above: allowed.
	if err := g.CheckBalance(HardFloorOctas + 10*GasOctasPerClaim); err != nil {
		t.Fatalf("a healthy balance was refused: %v", err)
	}
	// One claim would take it below the floor: refused BEFORE it happens.
	if err := g.CheckBalance(HardFloorOctas + GasOctasPerClaim - 1); !errors.Is(err, ErrLowBalance) {
		t.Fatalf("want ErrLowBalance, got %v", err)
	}
	// The message names BOTH numbers, because a contributor is the one who will
	// tell us and they need something to quote.
	//
	// The balance is deliberately not equal to the floor. An earlier version used
	// the floor as the balance, so "1.7009" appeared in the message from the
	// balance position and the assertion passed even when the floor was replaced
	// by a placeholder - the same shape as asserting "USDC" against a seeded
	// "USDC".
	err := g.CheckBalance(HardFloorOctas / 2) // 0.8504 APT, distinct from 1.7009
	if err == nil {
		t.Fatalf("a balance below the floor was allowed")
	}
	if !contains(err.Error(), "0.8504") {
		t.Errorf("the refusal does not state the balance: %v", err)
	}
	if !contains(err.Error(), "1.7009") {
		t.Errorf("the refusal does not state the floor: %v", err)
	}
}

func TestCheckRate_BoundsSubmissionsPerHour(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid, sid := seedUserAndSettlement(t, d)
	g := NewGuard(d.Pool)

	for i := 0; i < MaxSponsorshipsPerHour; i++ {
		if err := g.CheckRate(ctx, uid); err != nil {
			t.Fatalf("submission %d refused early: %v", i+1, err)
		}
		leaf := make([]byte, 32)
		leaf[0] = byte(i + 1)
		if err := g.Record(ctx, uid, sid, leaf, "submitted", "0xtx", GasOctasPerClaim); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.CheckRate(ctx, uid); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the %dth submission was allowed: %v", MaxSponsorshipsPerHour+1, err)
	}
}

// Refusals must not consume the rate budget: they cost no gas, and counting
// them would let an attacker lock a real contributor out by burning their quota.
func TestCheckRate_RefusalsDoNotConsumeTheBudget(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid, sid := seedUserAndSettlement(t, d)
	g := NewGuard(d.Pool)

	for i := 0; i < MaxSponsorshipsPerHour*3; i++ {
		leaf := make([]byte, 32)
		leaf[0] = byte(i + 1)
		if err := g.Record(ctx, uid, sid, leaf, "refused_simulation_failed", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.CheckRate(ctx, uid); err != nil {
		t.Fatalf("refusals consumed the rate budget: %v", err)
	}
}

// One submitted sponsorship per leaf, enforced by the index rather than counted:
// a second is either a double-click or an attempt to make us pay twice.
func TestRecord_RefusesASecondSubmissionForOneLeaf(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	uid, sid := seedUserAndSettlement(t, d)
	g := NewGuard(d.Pool)
	leaf := make([]byte, 32)
	leaf[0] = 9

	if err := g.Record(ctx, uid, sid, leaf, "submitted", "0xtx", GasOctasPerClaim); err != nil {
		t.Fatal(err)
	}
	if err := g.Record(ctx, uid, sid, leaf, "submitted", "0xtx2", GasOctasPerClaim); err == nil {
		t.Fatal("a second submission for the same leaf was recorded; we would have paid twice")
	}
	// A refusal for the same leaf is still recordable - it is how a burst shows.
	if err := g.Record(ctx, uid, sid, leaf, "refused_already_claimed", "", 0); err != nil {
		t.Fatalf("a refusal for an already-submitted leaf was rejected: %v", err)
	}
}

func seedUserAndSettlement(t *testing.T, d *db.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	uid, sid := uuid.New(), uuid.UUID{}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO users (id, role) VALUES ($1,'contributor')`, uid); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, pool_minor, asset_decimals)
		VALUES (1,1,1,1000,6) RETURNING id`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(ctx, `DELETE FROM sponsored_claims WHERE user_id=$1`, uid)
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE id=$1`, sid)
		d.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	})
	return uid, sid
}

func contains(h, n string) bool { return len(n) > 0 && len(h) >= len(n) && indexOf(h, n) >= 0 }
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
