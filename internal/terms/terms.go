// Package terms records which version of the Terms each person accepted.
//
// # Where the version comes from
//
// The text lives in the frontend (src/features/settings/components/terms),
// and so does the constant that names it, TERMS_VERSION. This package holds
// the same list, because the server must be able to refuse agreement to a
// version that was never published: a client that can record acceptance of
// any string it likes can record acceptance of a text nobody wrote.
//
// The two lists are kept in step by hand, and the order of a release matters:
// the backend learns a new version FIRST, then the frontend starts asking for
// it. The other order makes every acceptance fail with unknown_version until
// the backend catches up, which is loud and harmless, but pointless.
//
// # What a version is
//
// The date the text was published, YYYY-MM-DD. Dates sort as strings, so
// "is the accepted version older than the current one" is a string compare,
// and a person reading the database can tell what each row means without a
// lookup table.
package terms

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Versions is every version of the Terms ever published, oldest first. Never
// remove one: a row in terms_acceptances naming it must stay explainable.
//
// 2026-10-02 is the text that was live when acceptance was still a browser
// flag. It is listed so the record can say what an old client showed, but
// nobody was ever recorded as accepting it on the server.
var Versions = []string{
	"2026-10-02",
	"2026-10-03",
}

// Current is the version people are asked to accept now.
func Current() string { return Versions[len(Versions)-1] }

// Known reports whether v was ever published.
func Known(v string) bool {
	for _, k := range Versions {
		if k == v {
			return true
		}
	}
	return false
}

// ErrUnknownVersion is returned when a client asks to record a version this
// server never published.
var ErrUnknownVersion = errors.New("terms: unknown version")

// Pool is the subset of a connection pool this package needs; a pgxpool.Pool
// and a pgx.Tx both satisfy it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Status is what somebody is told about their own acceptance.
type Status struct {
	Current         string     `json:"current_version"`
	AcceptedVersion string     `json:"accepted_version"`
	AcceptedAt      *time.Time `json:"accepted_at"`
	// NeedsAcceptance is true when the latest version this person accepted is
	// older than the current one, including when they never accepted any.
	NeedsAcceptance bool `json:"needs_acceptance"`
}

// Get reads the most recent version this person accepted.
func Get(ctx context.Context, pool Pool, userID uuid.UUID) (Status, error) {
	s := Status{Current: Current()}
	var v string
	var at time.Time
	// "Most recent" by version, not by timestamp: somebody who accepted the
	// new text in one tab and then pressed Accept on an old cached page in
	// another has still agreed to the new text.
	err := pool.QueryRow(ctx, `
SELECT version, accepted_at FROM terms_acceptances
WHERE user_id = $1
ORDER BY version DESC
LIMIT 1
`, userID).Scan(&v, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Status{}, err
	}
	if err == nil {
		s.AcceptedVersion = v
		s.AcceptedAt = &at
	}
	s.NeedsAcceptance = s.AcceptedVersion < s.Current
	return s, nil
}

// Accept records that this person accepted version v. Accepting a version
// already accepted is a no-op that keeps the first timestamp.
func Accept(ctx context.Context, pool Pool, userID uuid.UUID, v string) (Status, error) {
	if !Known(v) {
		return Status{}, ErrUnknownVersion
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO terms_acceptances (user_id, version) VALUES ($1, $2)
ON CONFLICT (user_id, version) DO NOTHING
`, userID, v); err != nil {
		return Status{}, err
	}
	return Get(ctx, pool, userID)
}
