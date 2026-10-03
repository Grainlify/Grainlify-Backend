package erasure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

// Retention erases what is kept only for a fixed time, once that time is up.
//
// The Terms give each kept category a period ("kept for 90 days, then
// erased"). A period with nothing acting on it is a promise nobody keeps, so
// each one is a pass here, run daily, rather than a sentence alone.
//
// Each pass only removes rows already past their own period, and is safe to
// run again: a second run the same day finds nothing.
type Retention struct {
	pool     db.DBPool
	ext      External
	interval time.Duration
	now      func() time.Time
}

// ResetRecordRetention is how long the record of an administrator's
// identity-verification reset is kept: long enough to answer "was this
// person reset, by whom, and why" while a fraud question about it is still
// likely; then it is erased, for everybody, not only for erased accounts.
const ResetRecordRetention = 90 * 24 * time.Hour

// NewRetention builds the daily retention pass. ext is the same External the
// executor uses: a reset record names a Didit session, and erasing the record
// asks Didit to delete that session too.
func NewRetention(pool db.DBPool, ext External, interval time.Duration) *Retention {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Retention{pool: pool, ext: ext, interval: interval, now: time.Now}
}

// Run passes on the interval until the context ends. It passes once
// immediately, so a deploy does not postpone what is already due.
func (r *Retention) Run(ctx context.Context) {
	pass := func() {
		n, err := r.RunOnce(ctx)
		if err != nil {
			slog.Error("retention pass failed", "error", err)
		}
		var total int64
		for _, v := range n {
			total += v
		}
		if total > 0 {
			slog.Info("retention pass", "erased", n)
		}
	}
	pass()
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}

// RunOnce runs every retention step once and reports what each erased. A step
// that fails does not stop the others.
func (r *Retention) RunOnce(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	var firstErr error
	n, err := r.purgeResetRecords(ctx)
	out["kyc_reset_audit"] = n
	if err != nil {
		firstErr = err
	}
	return out, firstErr
}

// purgeResetRecords deletes kyc_reset_audit rows older than
// ResetRecordRetention.
//
// A reset record names the Didit session that was detached, and that session
// still holds the person's document data and images at Didit, which keeps it
// with no time limit unless we delete it. Deleting our record alone would
// orphan it there: nothing else names it, so not even erasing the account
// later could find it (Executor.diditSessions reads it from these rows). So
// the session is deleted at Didit first, and the row only once Didit has
// confirmed it, or answered that it is already gone.
//
// A session still in use - the person's current one, or named by a newer
// reset - is left at Didit; only the old row goes.
//
// When Didit cannot be reached, or is not configured, the row stays and the
// next pass tries again: a record kept a day late is a smaller harm than a
// session nobody can find again.
func (r *Retention) purgeResetRecords(ctx context.Context) (int64, error) {
	cutoff := r.now().Add(-ResetRecordRetention)
	rows, err := r.pool.Query(ctx, `
SELECT a.id, COALESCE(a.previous_session_id, ''),
       COALESCE(a.previous_session_id, '') <> '' AND (
         EXISTS (SELECT 1 FROM users u WHERE u.kyc_session_id = a.previous_session_id)
         OR EXISTS (SELECT 1 FROM kyc_reset_audit o
                    WHERE o.previous_session_id = a.previous_session_id AND o.id <> a.id AND o.created_at >= $1))
FROM kyc_reset_audit a
WHERE a.created_at < $1
ORDER BY a.created_at
LIMIT 500
`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention: list reset records: %w", err)
	}
	type due struct {
		id        uuid.UUID
		session   string
		stillUsed bool
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		err := row.Scan(&d.id, &d.session, &d.stillUsed)
		return d, err
	})
	if err != nil {
		return 0, fmt.Errorf("retention: list reset records: %w", err)
	}

	var erase []uuid.UUID
	var firstErr error
	for _, d := range list {
		if d.session != "" && !d.stillUsed {
			err := r.ext.DeleteDiditSession(ctx, d.session)
			switch {
			case err == nil, errors.Is(err, didit.ErrSessionNotFound):
			case errors.Is(err, ErrNotConfigured):
				slog.Warn("retention: reset record kept: Didit is not configured, so its session cannot be deleted", "audit_id", d.id)
				continue
			default:
				// Only the audit id: the session id is the key to the
				// person's verification at Didit.
				slog.Warn("retention: reset record kept: Didit session delete failed; retrying next pass", "audit_id", d.id, "error", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("retention: didit delete for reset record %s: %w", d.id, err)
				}
				continue
			}
		}
		erase = append(erase, d.id)
	}
	if len(erase) == 0 {
		return 0, firstErr
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM kyc_reset_audit WHERE id = ANY($1) AND created_at < $2`, erase, cutoff)
	if err != nil {
		return 0, fmt.Errorf("retention: delete reset records: %w", err)
	}
	return tag.RowsAffected(), firstErr
}
