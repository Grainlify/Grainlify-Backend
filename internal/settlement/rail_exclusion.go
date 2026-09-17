package settlement

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// The Aptos-side half of "an event is paid on one rail only".
//
// Migration 20260916090700 refuses a settlements row for an event and pool that
// already has a KeeperHub (Base) payout run. This file turns that refusal into
// something an operator can read.
//
// # Why the database raises a code of its own
//
// A bare constraint violation reaching `payout persist` reads as a bug - a
// schema that disagrees with the code - when it is a deliberate policy doing its
// job. So the trigger raises a stable, custom SQLSTATE, and this is the one
// place that recognises it and says what it means. Recognition is by code, not
// by message text: a message is prose and gets reworded; a code is an
// interface.

// RailExclusionSQLState is the SQLSTATE the trigger raises.
//
// Class "KH" sits in the range the SQL standard leaves to implementations (I-Z)
// and is not a class Postgres uses, so it cannot be mistaken for a real
// constraint violation.
const RailExclusionSQLState = "KH001"

// ErrEventPaidOnKeeperHub matches a settlement refused because the event is
// already being paid on the KeeperHub rail.
var ErrEventPaidOnKeeperHub = errors.New("settlement: event is already being paid on the KeeperHub rail")

// RailExclusionError is that refusal, with the event it concerns.
type RailExclusionError struct {
	HackathonID string
	Pool        string
	cause       error
}

// Error is the sentence an operator reads, and deliberately not the Postgres
// text: it says what the policy is, why it exists, and that nothing happened.
func (e *RailExclusionError) Error() string {
	return fmt.Sprintf("refused: hackathon %s (%s pool) is already being paid on the KeeperHub (Base) rail, "+
		"so it cannot also be settled on the Aptos rail. An event is paid on one rail only - "+
		"recording this settlement would pay the same people a second time. Nothing was written.",
		e.HackathonID, e.Pool)
}

// Unwrap keeps the database error reachable for logs, behind the sentinel.
func (e *RailExclusionError) Unwrap() []error { return []error{ErrEventPaidOnKeeperHub, e.cause} }

// asRailExclusion recognises the trigger's refusal, or returns nil.
func asRailExclusion(err error, res *Result, pool string) *RailExclusionError {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != RailExclusionSQLState {
		return nil
	}
	id := "unknown"
	if res != nil && res.HackathonID != nil {
		id = res.HackathonID.String()
	}
	return &RailExclusionError{HackathonID: id, Pool: pool, cause: err}
}
