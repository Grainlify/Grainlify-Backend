// Package syncqueue is how work gets into sync_jobs.
//
// Every enqueue site used to INSERT its own (sync_issues, sync_prs) pair
// unconditionally, so each webhook added two full-repository syncs on top of
// whatever was already waiting. In production that reached 38k pending jobs
// for 206 (project, job type) pairs. A pending job reads the repository when
// it starts, so a second pending job for the same pair can only see the same
// thing: Enqueue coalesces into it instead, against the unique partial index
// uq_sync_jobs_one_pending (migration 20261003122120).
//
// This package is a leaf - it imports nothing from the rest of the backend -
// so internal/syncjobs, internal/hackathon, internal/ingest and
// internal/handlers can all use it without an import cycle.
package syncqueue

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Job types. These are the values sync_jobs.job_type's CHECK accepts.
const (
	SyncIssues = "sync_issues"
	SyncPRs    = "sync_prs"
)

// AllTypes is the full-project sync: both job types.
var AllTypes = []string{SyncIssues, SyncPRs}

// WebhookDelay is how long a webhook-triggered sync waits before it may run.
//
// One action on GitHub is usually several deliveries - opening a pull request
// sends pull_request, push and often issue_comment within seconds - and the
// webhook path has already written the snapshot row from the payload, so the
// sync is a backstop rather than the thing the user is waiting on. Measured
// against a week of production webhooks, a 30 s window halves the number of
// syncs that run (about 2,450 a day with no delay, 1,180 with it). The delay
// counts from the first event, not the last, so a steady stream of webhooks
// cannot postpone a sync indefinitely.
const WebhookDelay = 30 * time.Second

// Execer is the one method Enqueue needs; *pgxpool.Pool, pgx.Tx and
// db.DBPool all satisfy it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue queues one job of each of jobTypes for projectID, eligible to run
// after delay - except for any type that already has a pending job for this
// project, which covers it. It returns how many rows were inserted.
//
// A job that is already RUNNING does not absorb the request: it may have read
// past whatever changed, so a new pending job is queued behind it (the worker
// never runs two jobs for the same project and type at once).
func Enqueue(ctx context.Context, db Execer, projectID uuid.UUID, delay time.Duration, jobTypes ...string) (int64, error) {
	if len(jobTypes) == 0 {
		return 0, nil
	}
	tag, err := db.Exec(ctx, `
INSERT INTO sync_jobs (project_id, job_type, status, run_at)
SELECT $1, t, 'pending', now() + make_interval(secs => $2)
FROM unnest($3::text[]) AS t
ON CONFLICT (project_id, job_type) WHERE status = 'pending' DO NOTHING
`, projectID, delay.Seconds(), jobTypes)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		Wake()
	}
	return tag.RowsAffected(), nil
}

var wake = make(chan struct{}, 1)

// Wake tells an in-process worker that sync_jobs may have something new, so
// it does not sleep out its idle backoff. Never blocks; wakes coalesce.
func Wake() {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Wakeups is the channel Wake signals.
func Wakeups() <-chan struct{} { return wake }
