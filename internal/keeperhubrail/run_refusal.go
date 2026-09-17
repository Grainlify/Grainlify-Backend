package keeperhubrail

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Recognising the database's refusal to open a KeeperHub run.
//
// Migration 20260916090300 refuses a KeeperHub run for an event already settled
// on the Aptos rail; 20260916090800 gave that refusal a code of its own. This
// is the one place that reads it.
//
// # By code, never by message
//
// This used to match the substring "already has a settlement" in the error
// text. That fails in both directions: reword the trigger's message and a real
// refusal stops being recognised, falling through to a generic "insert run"
// failure; and any other error that happens to contain those words is mistaken
// for the policy. A message is prose and gets edited. A SQLSTATE is an
// interface. TestRunRefusal_IsRecognisedByCodeNotByMessage pins it.

// RunRefusalSQLState is the code the keeperhub_payout_runs trigger raises.
//
// Distinct from settlement.RailExclusionSQLState (KH001), which is the refusal
// in the other direction, so the two can never be confused.
const RunRefusalSQLState = "KH002"

// SettledOnAptosError is that refusal, with the event it concerns. It matches
// errors.Is(err, ErrSettledOnAptos).
type SettledOnAptosError struct {
	HackathonID string
	Pool        string
	cause       error
}

// Error is the sentence an operator reads, not the database's text.
func (e *SettledOnAptosError) Error() string {
	return fmt.Sprintf("refused: hackathon %s (%s pool) is already settled on the Aptos rail, "+
		"so a KeeperHub run cannot be opened for it. An event is paid on one rail only - "+
		"opening this run would pay the same people a second time. Nothing was written.",
		e.HackathonID, e.Pool)
}

// Unwrap keeps the database error reachable for logs, behind the sentinel.
func (e *SettledOnAptosError) Unwrap() []error { return []error{ErrSettledOnAptos, e.cause} }

// runRefusal returns the typed refusal if err is the trigger's KH002, and nil
// otherwise - whatever the error's text says.
func runRefusal(err error, hackathonID, pool string) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != RunRefusalSQLState {
		return nil
	}
	return &SettledOnAptosError{HackathonID: hackathonID, Pool: pool, cause: err}
}
