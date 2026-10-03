package keeperhubrail

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// The GrainHack (Solana) rail is the third: a results statement for the event
// pool refuses a KeeperHub run, with GH002, recognised by code like KH002.
func TestRunRefusal_GrainHackStatementIsRecognisedByCode(t *testing.T) {
	hid := uuid.NewString()
	got := runRefusal(&pgconn.PgError{Code: "GH002", Message: "reworded"}, hid, "contributor")
	if !errors.Is(got, ErrPaidOnGrainHack) {
		t.Fatalf("GH002 not recognised: %v", got)
	}
	if errors.Is(got, ErrSettledOnAptos) {
		t.Fatal("GH002 taken for the Aptos refusal")
	}
	if RefusalReason(got) != ReasonPaidOnGrainHack {
		t.Fatalf("reason = %q", RefusalReason(got))
	}
}

// insertStatement records a GrainHack results statement for the fixture's
// event directly - one line holding the whole pool, as the sum check needs.
func insertStatement(t *testing.T, f *fx) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		f.d.Pool.Exec(context.Background(), `TRUNCATE grainhack_notices, grainhack_payment_reports,
			grainhack_results_statement_lines, grainhack_results_statements`)
	})
	var ghID int64
	var login string
	if err := f.d.Pool.QueryRow(ctx, `SELECT github_user_id, login FROM github_accounts WHERE user_id = $1`,
		f.paid[0]).Scan(&ghID, &login); err != nil {
		t.Fatal(err)
	}
	tx, err := f.d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	sid := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO grainhack_results_statements
		  (id, hackathon_id, pool, computation_id, currency, network, pool_minor,
		   canonical_json, signature, signing_public_key, issued_by, issued_at)
		VALUES ($1, $2, 'contributor', $3, 'USDC', 'solana-devnet', 7000000, 'x', 'x', 'x', $4, now())`,
		sid, f.hid, f.payoutRun, f.actor); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO grainhack_results_statement_lines (statement_id, github_user_id, user_id, login, amount_minor, status)
		VALUES ($1, $2, $3, $4, 7000000, 'payable')`, sid, ghID, f.paid[0], login); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestRunRefusal_GrainHackTriggerRaisesTheCodeTheRailReads(t *testing.T) {
	f := fixture(t)
	insertStatement(t, f)
	_, err := f.d.Pool.Exec(context.Background(), `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id)
		VALUES ($1, 'contributor', $2, $3, 7000000, $4)`, f.hid, f.chain, f.evmChainID, f.payoutRun)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "GH002" || pg.ConstraintName != "keeperhub_payout_runs_one_rail_per_event_pool" {
		t.Fatalf("err = %v, want GH002 from the one-rail trigger", err)
	}
	var gx *PaidOnGrainHackError
	if got := runRefusal(err, f.hid.String(), "contributor"); !errors.As(got, &gx) || gx.HackathonID != f.hid.String() {
		t.Fatalf("got %#v, want a PaidOnGrainHackError naming the event", got)
	}
}

// Release refuses in Go, before planning or calling KeeperHub, when the event
// pool has a statement - and the admin screen's reason says why.
func TestRelease_RefusesAnEventPaidByGrainHackStatement(t *testing.T) {
	f := fixture(t)
	insertStatement(t, f)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	_, err := s.Release(context.Background(), f.req())
	if !errors.Is(err, ErrPaidOnGrainHack) {
		t.Fatalf("err = %v, want ErrPaidOnGrainHack", err)
	}
	if RefusalReason(err) != ReasonPaidOnGrainHack {
		t.Fatalf("reason = %q", RefusalReason(err))
	}
	if len(rail.calls) != 0 {
		t.Fatal("a refused release reached KeeperHub")
	}
	if n := f.count(t, `SELECT count(*) FROM keeperhub_payout_runs WHERE hackathon_id = $1`); n != 0 {
		t.Fatalf("a refused release planned %d run(s)", n)
	}
}
