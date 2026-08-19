// Package expiry deletes rows whose expires_at has passed.
//
// # Why this exists
//
// A table with an expires_at column and nothing that acts on it is a table that
// grows forever. `oauth_states` is the worked example: 242 rows in production at
// the time of writing, one per abandoned sign-in attempt since the column was
// added, none of them ever read again and none of them ever removed.
//
// Address registration issues a nonce per attempt, and most attempts will not
// complete - somebody opens the dialog, sees the wallet prompt, and closes it.
// Without this, `auth_nonces` inherits exactly the same shape, on a table whose
// rows are single-use by design.
//
// This is the third instance of one family in the codebase: a column that exists
// and is never written (merged_by), a column that is populated and never read
// (oauth_states.expires_at), and a config table read by code and written by no
// migration (chain_configs). Each is a schema and a codebase disagreeing about
// who is responsible for a value.
//
// # What it does not do
//
// It never deletes an unexpired row, and it does not decide what "expired" means
// - each table's own expires_at does. There is no retention policy here beyond
// the one the schema already states.
package expiry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Table is one table to sweep. Listed rather than discovered, because deleting
// rows from a table nobody intended is worse than leaving rows in one.
type Table struct {
	Name   string
	Column string
}

// Tables is the set swept on every pass.
//
// auth_nonces is the reason this package exists; oauth_states is the table that
// demonstrated the need. Both are single-use tokens whose rows have no meaning
// after their expiry.
var Tables = []Table{
	{Name: "auth_nonces", Column: "expires_at"},
	{Name: "oauth_states", Column: "expires_at"},
}

// Grace is how far past expiry a row is kept.
//
// Not zero, deliberately. A nonce that expires between a client sending it and
// the server reading it should produce "expired", which is a fact somebody can
// act on, rather than "unknown", which reads as though the request was never
// made. The distinction only survives if the row is still there to be found.
const Grace = 24 * time.Hour

// Sweeper deletes expired rows on an interval.
type Sweeper struct {
	pool     db.DBPool
	interval time.Duration
}

func New(pool db.DBPool, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Sweeper{pool: pool, interval: interval}
}

// SweepOnce deletes expired rows from every table and reports how many per
// table. Safe to call concurrently with anything; it only removes rows that are
// already past their own expiry plus the grace period.
func (s *Sweeper) SweepOnce(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(Tables))
	for _, t := range Tables {
		// The table and column are from the fixed list above, never from input.
		q := fmt.Sprintf(`DELETE FROM %s WHERE %s < now() - $1::interval`, t.Name, t.Column)
		tag, err := s.pool.Exec(ctx, q, fmt.Sprintf("%d seconds", int(Grace.Seconds())))
		if err != nil {
			return out, fmt.Errorf("expiry: sweep %s: %w", t.Name, err)
		}
		out[t.Name] = tag.RowsAffected()
	}
	return out, nil
}

// Run sweeps on the interval until the context is cancelled. It sweeps once
// immediately, so a deploy clears whatever accumulated while nothing was
// sweeping rather than waiting an hour to start.
func (s *Sweeper) Run(ctx context.Context) {
	sweep := func() {
		n, err := s.SweepOnce(ctx)
		if err != nil {
			slog.Error("expiry sweep failed", "error", err)
			return
		}
		total := int64(0)
		for _, v := range n {
			total += v
		}
		if total > 0 {
			slog.Info("expiry sweep", "deleted", n, "total", total)
		}
	}
	sweep()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
