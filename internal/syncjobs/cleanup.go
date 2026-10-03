package syncjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// CleanupReport is what Cleanup found and, when applied, changed.
type CleanupReport struct {
	Applied bool
	// Before and After count sync_jobs rows by "status/job_type". After is
	// empty on a dry run.
	Before, After map[string]int64
	// Planned counts the rows each action touches (or would touch).
	Planned map[string]int64
}

// Cleanup actions, as reported in CleanupReport.Planned.
const (
	ActionCancelDuplicatePending = "cancel_duplicate_pending"
	ActionRequeueStuckRunning    = "requeue_stuck_running"
	ActionCancelStuckRunning     = "cancel_stuck_running"
)

// cleanupPlan lists every row the cleanup changes and what it becomes. It is
// the one definition both the dry run and the apply read, so the numbers a
// dry run prints are the rows an apply touches.
//
//   - (a) Every pending job but the oldest of its (project, job type) is
//     cancelled: the oldest runs first and reads the same repository.
//   - (b) A job 'running' with no lease renewal for $1 seconds belongs to a
//     worker that is gone (each production one was locked_by a container
//     that no longer exists). The newest such job of a pair goes back to
//     pending if no pending job covers the pair; every other is cancelled.
//
// Rows are marked, not deleted: the reason says what happened and when.
const cleanupPlan = `
WITH pending_ranked AS (
  SELECT id, project_id, job_type,
         row_number() OVER (PARTITION BY project_id, job_type ORDER BY run_at, created_at, id) AS rn
  FROM sync_jobs
  WHERE status = 'pending'
),
stuck AS (
  SELECT id, project_id, job_type, COALESCE(locked_at, updated_at) AS since, COALESCE(locked_by, 'unknown') AS worker,
         row_number() OVER (PARTITION BY project_id, job_type ORDER BY COALESCE(locked_at, updated_at) DESC, id) AS rn
  FROM sync_jobs
  WHERE status = 'running' AND COALESCE(locked_at, updated_at) < now() - make_interval(secs => $1::float8)
)
SELECT id, 'cancelled' AS new_status, 'cancel_duplicate_pending' AS action,
       'coalesced: another pending job for this project and job type already covers it (sync jobs cleanup)' AS reason
FROM pending_ranked WHERE rn > 1
UNION ALL
SELECT s.id,
       CASE WHEN covered THEN 'cancelled' ELSE 'pending' END,
       CASE WHEN covered THEN 'cancel_stuck_running' ELSE 'requeue_stuck_running' END,
       'reset: running since ' || to_char(s.since AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') ||
         ' under worker ' || s.worker || ', which is gone' ||
         CASE WHEN covered THEN '; superseded by the job already pending' ELSE '' END ||
         ' (sync jobs cleanup)'
FROM (
  SELECT s.*, (s.rn > 1 OR EXISTS (
    SELECT 1 FROM pending_ranked p WHERE p.project_id = s.project_id AND p.job_type = s.job_type
  )) AS covered
  FROM stuck s
) s
`

// Cleanup collapses the pending backlog to one job per (project, job type)
// and takes back jobs stuck in 'running' longer than stuckAfter. With apply
// false it only reads, in a READ ONLY transaction, and reports what it would
// do. With apply true it does it in one transaction, holding a lock that keeps
// new jobs out until it commits. Running it again finds nothing to do.
//
// It works on either side of migration 20261003122120: before it, the
// 'cancelled' status does not exist yet, and an apply adds it with exactly
// the CHECK that migration installs.
func Cleanup(ctx context.Context, pool db.DBPool, stuckAfter time.Duration, apply bool) (CleanupReport, error) {
	r := CleanupReport{Applied: apply, Planned: map[string]int64{}}
	opts := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	if apply {
		opts = pgx.TxOptions{}
	}
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if apply {
		// Kept short: if the worker or a webhook holds the table this long,
		// fail and let the operator retry rather than queue everyone behind.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			return r, err
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE sync_jobs IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return r, fmt.Errorf("lock sync_jobs: %w", err)
		}
	}

	if r.Before, err = countJobs(ctx, tx); err != nil {
		return r, err
	}

	rows, err := tx.Query(ctx, `SELECT action, count(*) FROM (`+cleanupPlan+`) plan GROUP BY action`, stuckAfter.Seconds())
	if err != nil {
		return r, fmt.Errorf("plan: %w", err)
	}
	for rows.Next() {
		var action string
		var n int64
		if err := rows.Scan(&action, &n); err != nil {
			rows.Close()
			return r, err
		}
		r.Planned[action] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return r, err
	}

	if !apply {
		return r, nil
	}

	var def string
	if err := tx.QueryRow(ctx, `
SELECT COALESCE((SELECT pg_get_constraintdef(oid) FROM pg_constraint
                 WHERE conrelid = 'sync_jobs'::regclass AND conname = 'sync_jobs_status_check'), '')`).Scan(&def); err != nil {
		return r, err
	}
	if !strings.Contains(def, "cancelled") {
		if _, err := tx.Exec(ctx, `
ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS sync_jobs_status_check;
ALTER TABLE sync_jobs ADD CONSTRAINT sync_jobs_status_check
  CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled'));`); err != nil {
			return r, fmt.Errorf("widen status check: %w", err)
		}
	}

	tag, err := tx.Exec(ctx, `
UPDATE sync_jobs j
SET status     = plan.new_status,
    last_error = plan.reason,
    attempts   = j.attempts + CASE WHEN j.status = 'running' THEN 1 ELSE 0 END,
    run_at     = CASE WHEN j.status = 'running' AND plan.new_status = 'pending' THEN now() ELSE j.run_at END,
    locked_at  = NULL,
    locked_by  = NULL,
    updated_at = now()
FROM (`+cleanupPlan+`) plan
WHERE j.id = plan.id
`, stuckAfter.Seconds())
	if err != nil {
		return r, fmt.Errorf("apply: %w", err)
	}
	var planned int64
	for _, n := range r.Planned {
		planned += n
	}
	if tag.RowsAffected() != planned {
		return r, fmt.Errorf("apply touched %d rows but the plan listed %d; rolled back", tag.RowsAffected(), planned)
	}

	if r.After, err = countJobs(ctx, tx); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

func countJobs(ctx context.Context, tx pgx.Tx) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `SELECT status || '/' || job_type, count(*) FROM sync_jobs GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
