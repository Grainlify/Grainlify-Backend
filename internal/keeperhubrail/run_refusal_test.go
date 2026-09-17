package keeperhubrail

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The one-rail refusal is recognised by its SQLSTATE, and by nothing else.
//
// A discriminating pair: if recognition depended on the message, (1) would fail
// because the words changed, and (2) would pass because the words match. The
// assertion is literally "changing the message text does not change behaviour".
func TestRunRefusal_IsRecognisedByCodeNotByMessage(t *testing.T) {
	hid := uuid.NewString()

	t.Run("same code, reworded message: still the rail refusal", func(t *testing.T) {
		err := &pgconn.PgError{
			Code:    "KH002",
			Message: "this event is settled elsewhere, go away", // shares no words with the trigger's text
		}
		got := runRefusal(err, hid, "contributor")
		if !errors.Is(got, ErrSettledOnAptos) {
			t.Fatalf("a KH002 refusal with different wording was not recognised (got %v) - "+
				"recognition is depending on the message text", got)
		}
	})

	t.Run("same message, different code: not the rail refusal", func(t *testing.T) {
		err := &pgconn.PgError{
			Code:    "23000", // a generic integrity violation
			Message: "hackathon x pool contributor already has a settlement: it is being paid on the Aptos rail",
		}
		if got := runRefusal(err, hid, "contributor"); got != nil {
			t.Fatalf("an unrelated error was taken for the rail refusal because its words matched: %v", got)
		}
	})

	t.Run("not a database error at all: not the rail refusal", func(t *testing.T) {
		if got := runRefusal(errors.New("already has a settlement"), hid, "contributor"); got != nil {
			t.Fatalf("a plain error with the right words was taken for the rail refusal: %v", got)
		}
	})
}

// End to end against the real trigger: an event settled on Aptos refuses a
// KeeperHub run with KH002, and that is what the rail recognises.
//
// Inserted directly rather than through Release, because Release checks for a
// settlement in Go first and never reaches the trigger unless it loses a race -
// and a test that has to win a race to reach a branch passes for the wrong
// reason.
func TestRunRefusal_TheTriggerRaisesTheCodeTheRailReads(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	if _, err := f.d.Pool.Exec(ctx, `
		INSERT INTO settlements (hackathon_id, pool, pool_usdc, total_weight, unit_value_usdc, pool_minor, asset_decimals)
		VALUES ($1, 'contributor', 7, 7, 1, 7000000, 6)`, f.hid); err != nil {
		t.Fatal(err)
	}
	_, err := f.d.Pool.Exec(ctx, `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id)
		VALUES ($1, 'contributor', $2, $3, 7000000, $4)`, f.hid, f.chain, f.evmChainID, f.payoutRun)
	if err == nil {
		t.Fatal("the trigger allowed a KeeperHub run for an event already settled on Aptos")
	}

	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("not a database error: %T %v", err, err)
	}
	if pg.Code != "KH002" {
		t.Errorf("SQLSTATE = %q, want KH002", pg.Code)
	}
	if pg.ConstraintName != "keeperhub_payout_runs_one_rail_per_event_pool" {
		t.Errorf("constraint = %q", pg.ConstraintName)
	}
	if pg.Detail == "" || pg.Hint == "" {
		t.Errorf("detail %q / hint %q, want both set", pg.Detail, pg.Hint)
	}

	got := runRefusal(err, f.hid.String(), "contributor")
	if !errors.Is(got, ErrSettledOnAptos) {
		t.Fatalf("the trigger's refusal was not recognised: %v", got)
	}
	var rx *SettledOnAptosError
	if !errors.As(got, &rx) || rx.HackathonID != f.hid.String() || rx.Pool != "contributor" {
		t.Fatalf("got %#v, want a SettledOnAptosError naming the event and pool", got)
	}
}
