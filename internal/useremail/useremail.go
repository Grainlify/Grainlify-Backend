// Package useremail owns the one set of rules about a contributor's stored
// email address: when it may be written, when it may not, and who may read it.
//
// It is a package rather than a few lines inside the OAuth handler because
// the rules are the point. An address captured at login without the refusal
// check would come back the day after somebody removed it; an address read by
// a handler that does not know the rules would end up in a maintainer's
// applicant list. Both are one careless line away in any file that touches
// the users table, and neither shows up as a failure.
package useremail

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pool is the subset of the connection pool this package needs. *pgxpool.Pool
// and db.DBPool both satisfy it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Status is what the settings screen shows somebody about their own address.
type Status struct {
	// Address is the stored address, or "" when none is on file.
	Address string `json:"address"`
	// CapturedAt is when it was last written, RFC3339, or "" when none.
	CapturedAt string `json:"captured_at"`
	// Enabled is the master switch. False means no email from us at all,
	// whatever the per-type preferences say. In-app is unaffected.
	Enabled bool `json:"enabled"`
	// Declined is true once somebody has removed their address. While it is
	// true, signing in again does not put the address back.
	Declined bool `json:"declined"`
}

// Capture stores the address GitHub gave us at login.
//
// It does nothing at all when the person has removed their address before:
// a login is not consent to undo a deletion, and re-capturing here is the one
// behaviour that would make the remove button a lie. It also does nothing for
// an empty address, so a GitHub account with no public primary address does
// not blank out one we already hold.
//
// Returns true when the stored address changed.
func Capture(ctx context.Context, pool Pool, userID uuid.UUID, address string) (bool, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return false, nil
	}
	tag, err := pool.Exec(ctx, `
UPDATE users
SET email = $2, email_captured_at = now(), updated_at = now()
WHERE id = $1
  AND email_declined_at IS NULL
  AND (email IS DISTINCT FROM $2)
`, userID, address)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Get reads somebody's own address and switches.
func Get(ctx context.Context, pool Pool, userID uuid.UUID) (Status, error) {
	var s Status
	var address, capturedAt *string
	var declinedAt *string
	err := pool.QueryRow(ctx, `
SELECT email,
       to_char(email_captured_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
       email_notifications_enabled,
       to_char(email_declined_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
FROM users WHERE id = $1
`, userID).Scan(&address, &capturedAt, &s.Enabled, &declinedAt)
	if err != nil {
		return Status{}, err
	}
	if address != nil {
		s.Address = *address
	}
	if capturedAt != nil {
		s.CapturedAt = *capturedAt
	}
	s.Declined = declinedAt != nil
	return s, nil
}

// SetEnabled flips the master switch. It does not touch the stored address:
// turning email off and deleting the address are different things somebody
// might want, and conflating them would mean you cannot pause email without
// losing the address, or keep the address without accepting email.
func SetEnabled(ctx context.Context, pool Pool, userID uuid.UUID, enabled bool) error {
	_, err := pool.Exec(ctx, `
UPDATE users SET email_notifications_enabled = $2, updated_at = now() WHERE id = $1
`, userID, enabled)
	return err
}

// Remove deletes the stored address and records the refusal, so the next
// login does not put it back.
func Remove(ctx context.Context, pool Pool, userID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
UPDATE users
SET email = NULL, email_captured_at = NULL, email_declined_at = now(), updated_at = now()
WHERE id = $1
`, userID)
	return err
}

// Allow undoes a previous removal, so the next login captures the address
// again. Nothing else clears email_declined_at - a refusal is only ever
// reversed by the person who made it.
func Allow(ctx context.Context, pool Pool, userID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
UPDATE users SET email_declined_at = NULL, updated_at = now() WHERE id = $1
`, userID)
	return err
}
