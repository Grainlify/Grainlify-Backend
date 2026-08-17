package founding

import (
	"context"
	"errors"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The gate: one switch, read by entry and by payment.

func gateCfg(on bool) map[string]string {
	c := defaults()
	if on {
		c["founding_require_social_follow"] = "true"
	} else {
		c["founding_require_social_follow"] = "false"
	}
	return c
}

// The property the whole design turns on: entry and payment must never
// disagree about whether the gate is on. Expressed twice they drift, and the
// drift is worst at the moment somebody flips the switch - a gate that is
// unconditional on entry while payment reads a flag would, when the flag goes
// false, leave entry restricted and payment open.
func TestGate_EntryAndPaymentAgreeOnEveryState(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	resetFounding(t, d)

	for _, tc := range []struct {
		name    string
		status  string // "" means no submission at all
		gateOn  bool
		wantIn  bool
		wantPay bool
	}{
		{"approved, gate on", "approved", true, true, true},
		{"approved, gate off", "approved", false, true, true},
		{"never submitted, gate on", "", true, false, false},
		{"never submitted, gate off", "", false, true, true},
		{"pending, gate on", "pending", true, false, false},
		{"pending, gate off", "pending", false, true, true},
		{"revoked, gate on", "revoked", true, false, false},
		{"revoked, gate off", "revoked", false, true, true},
		{"rejected, gate on", "rejected", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := newUser(t, d)
			if tc.status != "" {
				socialFollowWithStatus(t, d, user, tc.status)
			}
			cfg := gateCfg(tc.gateOn)

			gotIn, _, err := approvedForFoundingPool(ctx, d.Pool, user, cfg)
			if err != nil {
				t.Fatalf("entry check: %v", err)
			}
			gotPay, _, err := Eligible(ctx, d.Pool, user, cfg)
			if err != nil {
				t.Fatalf("payment check: %v", err)
			}

			if gotIn != tc.wantIn {
				t.Errorf("entry = %v, want %v", gotIn, tc.wantIn)
			}
			if gotPay != tc.wantPay {
				t.Errorf("payment = %v, want %v", gotPay, tc.wantPay)
			}
			// The invariant, stated separately from the table so a wrong
			// expectation in the table cannot hide it.
			if gotIn != gotPay {
				t.Errorf("entry (%v) and payment (%v) disagree - they are meant to be one rule", gotIn, gotPay)
			}
		})
	}
}

// Verifying without an approval assigns nothing, and says so as an ordinary
// outcome rather than a failure.
func TestAssignWave_RefusesWithoutApproval(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	resetFounding(t, d)
	user := newUser(t, d)

	_, err := AssignWave(ctx, d.Pool, user, gateCfg(true))
	if !errors.Is(err, ErrNotEligibleForWave) {
		t.Fatalf("err = %v, want ErrNotEligibleForWave", err)
	}
	if _, ok, _ := MembershipFor(ctx, d.Pool, user); ok {
		t.Error("a wave was assigned to somebody with no approved submission")
	}
}

// Whichever of (verify, approve) happens second does the work. Both orders,
// because a gate that only admits one of them strands everybody in the other.
func TestAssignWave_EitherOrderEventuallyAssigns(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	resetFounding(t, d)
	cfg := gateCfg(true)

	t.Run("approve then verify", func(t *testing.T) {
		user := newUser(t, d)
		socialFollowWithStatus(t, d, user, "approved")
		m, err := AssignWave(ctx, d.Pool, user, cfg)
		if err != nil {
			t.Fatalf("AssignWave: %v", err)
		}
		if m.Sequence <= 0 {
			t.Errorf("sequence = %d, want a real position", m.Sequence)
		}
	})

	t.Run("verify then approve", func(t *testing.T) {
		user := newUser(t, d)
		// Verification first: refused, no position.
		if _, err := AssignWave(ctx, d.Pool, user, cfg); !errors.Is(err, ErrNotEligibleForWave) {
			t.Fatalf("first attempt err = %v, want ErrNotEligibleForWave", err)
		}
		// Approval second: assigns.
		socialFollowWithStatus(t, d, user, "approved")
		m, err := AssignWave(ctx, d.Pool, user, cfg)
		if err != nil {
			t.Fatalf("second attempt: %v", err)
		}
		if m.Sequence <= 0 {
			t.Errorf("sequence = %d, want a real position", m.Sequence)
		}
	})
}

// Existing members keep their number, unexamined. This is what makes
// "permanent" true and is why the short-circuit sits BEFORE the gate.
func TestAssignWave_ExistingMemberKeepsPositionEvenIfNowIneligible(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	resetFounding(t, d)
	cfg := gateCfg(true)

	user := newUser(t, d)
	socialFollowWithStatus(t, d, user, "approved")
	first, err := AssignWave(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("AssignWave: %v", err)
	}

	// Their approval is withdrawn afterwards.
	if _, err := d.Pool.Exec(ctx, `UPDATE social_follow_submissions SET status='revoked' WHERE user_id=$1`, user); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	again, err := AssignWave(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("re-assign after revoke: %v", err)
	}
	if again.Sequence != first.Sequence {
		t.Errorf("sequence moved from %d to %d - an assigned position must never change", first.Sequence, again.Sequence)
	}
	// But they no longer get paid: entry is permanent, payment is not.
	if ok, _, _ := Eligible(ctx, d.Pool, user, cfg); ok {
		t.Error("a revoked member is still eligible for payment")
	}
}

// Assignment is idempotent across repeated calls from both events.
func TestAssignWave_SecondEventIsANoOp(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)
	resetFounding(t, d)
	cfg := gateCfg(true)

	user := newUser(t, d)
	socialFollowWithStatus(t, d, user, "approved")

	first, err := AssignWave(ctx, d.Pool, user, cfg)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := AssignWave(ctx, d.Pool, user, cfg)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if again.Sequence != first.Sequence {
			t.Fatalf("repeat %d moved the sequence: %d -> %d", i, first.Sequence, again.Sequence)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*)::int FROM founding_members WHERE user_id=$1`, user).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("founding_members rows = %d, want exactly 1", n)
	}
}
