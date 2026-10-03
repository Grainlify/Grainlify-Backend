package syncjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/syncqueue"
)

// queueTiming is how the worker paces itself. The defaults below come from
// production's sync_jobs as of 2026-10-03 (completed jobs, last 14 days):
//
//	sync_issues  p50 39 s   p90 464 s   p99 582 s   max 674 s
//	sync_prs     p50  7 s   p90  56 s   p99 104 s   max 153 s
//
// A job's duration is therefore no basis for deciding it is stuck - a healthy
// sync_issues can run eleven minutes. Liveness is a heartbeat instead: the
// worker renews locked_at every heartbeat while the job runs, and a job whose
// lease has not been renewed for leaseTimeout belongs to a worker that is gone.
// In production that is every deploy: 111 jobs were 'running' with nobody
// running them, each locked_by a different container hostname, the oldest
// since 2026-01-02.
type queueTiming struct {
	// minIdle is the poll interval.
	minIdle      time.Duration
	heartbeat    time.Duration
	leaseTimeout time.Duration
	reapEvery    time.Duration
	// jobDeadline bounds a single job even while it heartbeats - a hung
	// sync must not hold its (project, type) forever. 5x the longest
	// observed job.
	jobDeadline time.Duration
}

var defaultTiming = queueTiming{
	minIdle:      1 * time.Second,
	heartbeat:    30 * time.Second,
	leaseTimeout: 5 * time.Minute,
	reapEvery:    1 * time.Minute,
	jobDeadline:  1 * time.Hour,
}

// maxAttempts caps how often a job is put back after it did not finish
// (lease expired, rate limited). A job that kills its worker every time it
// runs would otherwise be retried forever.
const maxAttempts = 5

func (w *Worker) t() queueTiming {
	t := w.timing
	d := defaultTiming
	if t.minIdle <= 0 {
		t.minIdle = d.minIdle
	}
	if t.heartbeat <= 0 {
		t.heartbeat = d.heartbeat
	}
	if t.leaseTimeout <= 0 {
		t.leaseTimeout = d.leaseTimeout
	}
	if t.reapEvery <= 0 {
		t.reapEvery = d.reapEvery
	}
	if t.jobDeadline <= 0 {
		t.jobDeadline = d.jobDeadline
	}
	return t
}

type claimedJob struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	JobType   string
	Attempts  int
}

var (
	errLeaseLost = errors.New("sync job lease lost: another worker or the reaper took it back")
)

// Run claims and runs jobs until ctx is cancelled. A job in flight when ctx
// is cancelled is put back to pending rather than left 'running'.
func (w *Worker) Run(ctx context.Context) error {
	if w.pool == nil {
		return fmt.Errorf("db not configured")
	}
	tm := w.t()
	var nextReap time.Time
	t := time.NewTicker(tm.minIdle)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}

		if now := time.Now(); !now.Before(nextReap) {
			if n, err := w.reapExpired(ctx); err != nil {
				slog.Error("sync worker: reaping expired jobs failed", "error", err)
			} else if n > 0 {
				slog.Warn("sync worker: took back jobs whose worker stopped heartbeating", "count", n)
			}
			nextReap = now.Add(tm.reapEvery)
		}

		if _, err := w.processOne(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sync worker error", "error", err)
		}
	}
}

// claim takes the oldest due pending job, skipping any (project, type) that
// already has a job running: two syncs of the same repository at once would
// only race each other's writes.
func (w *Worker) claim(ctx context.Context) (claimedJob, bool, error) {
	var j claimedJob
	err := w.pool.QueryRow(ctx, `
UPDATE sync_jobs
SET status = 'running', locked_at = now(), locked_by = $1, updated_at = now()
WHERE id = (
  SELECT p.id FROM sync_jobs p
  WHERE p.status = 'pending'
    AND p.run_at <= now()
    AND NOT EXISTS (
      SELECT 1 FROM sync_jobs r
      WHERE r.project_id = p.project_id AND r.job_type = p.job_type AND r.status = 'running'
    )
  ORDER BY p.run_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, project_id, job_type, attempts
`, w.workerID).Scan(&j.ID, &j.ProjectID, &j.JobType, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedJob{}, false, nil
	}
	if err != nil {
		return claimedJob{}, false, err
	}
	return j, true, nil
}

// processOne claims one job and runs it to an outcome. ran reports whether
// there was a job.
func (w *Worker) processOne(ctx context.Context) (ran bool, err error) {
	j, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	tm := w.t()

	jobCtx, cancelJob := context.WithCancelCause(ctx)
	jobCtx, cancelDeadline := context.WithTimeout(jobCtx, tm.jobDeadline)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		w.heartbeat(jobCtx, j.ID, tm.heartbeat, cancelJob)
	}()

	exec := w.exec
	if exec == nil {
		exec = w.runJob
	}
	runErr := exec(jobCtx, j)
	cause := context.Cause(jobCtx)
	cancelDeadline()
	cancelJob(nil)
	<-hbDone

	// The outcome is written even when ctx is already cancelled (shutdown):
	// writing it with ctx is what left jobs 'running' forever.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return true, w.finish(fctx, j, runErr, cause, ctx.Err() != nil)
}

func (w *Worker) finish(ctx context.Context, j claimedJob, runErr, cause error, shuttingDown bool) error {
	switch {
	case errors.Is(cause, errLeaseLost):
		// Someone else already decided this job's fate; writing ours would
		// overwrite theirs.
		slog.Warn("sync job lost its lease while running", "job_id", j.ID, "project_id", j.ProjectID, "job_type", j.JobType)
		return nil
	case shuttingDown:
		// Not the job's fault: put it back as it was, without spending an
		// attempt, so the next instance picks it up at once.
		_, err := w.requeue(ctx, j.ID, time.Now(), "released: worker shut down mid-job", 0, w.workerID, 0)
		return err
	case runErr == nil:
		_, err := w.pool.Exec(ctx, `
UPDATE sync_jobs
SET status = 'completed', attempts = attempts + 1, last_error = NULL, updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2
`, j.ID, w.workerID)
		return err
	}
	_, err := w.pool.Exec(ctx, `
UPDATE sync_jobs
SET status = 'failed', attempts = attempts + 1, last_error = $3, updated_at = now()
WHERE id = $1 AND status = 'running' AND locked_by = $2
`, j.ID, w.workerID, runErr.Error())
	return err
}

// heartbeat renews the job's lease until ctx ends. If the renewal finds the
// job is no longer running under this worker, the job is cancelled.
func (w *Worker) heartbeat(ctx context.Context, id uuid.UUID, every time.Duration, cancelJob context.CancelCauseFunc) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tag, err := w.pool.Exec(ctx, `
UPDATE sync_jobs SET locked_at = now() WHERE id = $1 AND status = 'running' AND locked_by = $2
`, id, w.workerID)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("sync job heartbeat failed", "job_id", id, "error", err)
			}
			continue
		}
		if tag.RowsAffected() == 0 {
			cancelJob(errLeaseLost)
			return
		}
	}
}

// requeue moves a running job back to pending, eligible at runAt - or, if
// its (project, type) already has a pending job that will do the same work,
// cancels it and makes sure that pending job does not run before runAt. Only
// a job still running and matching the guard is touched: lockedBy (when not
// empty) must hold the lease, and staleFor (when not zero) is how long its
// lease must have gone unrenewed. Returns the status the job ended up in, or
// "" if the guard no longer matched.
func (w *Worker) requeue(ctx context.Context, id uuid.UUID, runAt time.Time, reason string, addAttempts int, lockedBy string, staleFor time.Duration) (string, error) {
	var status string
	err := w.pool.QueryRow(ctx, `
WITH me AS (
  SELECT id, project_id, job_type FROM sync_jobs
  WHERE id = $1 AND status = 'running'
    AND ($5::text = '' OR locked_by = $5::text)
    AND ($6::float8 = 0 OR COALESCE(locked_at, updated_at) < now() - make_interval(secs => $6::float8))
  FOR UPDATE
),
sibling AS (
  UPDATE sync_jobs s SET run_at = GREATEST(s.run_at, $2::timestamptz), updated_at = now()
  FROM me
  WHERE s.project_id = me.project_id AND s.job_type = me.job_type AND s.status = 'pending' AND s.id <> me.id
  RETURNING s.id
)
UPDATE sync_jobs j
SET status     = CASE WHEN EXISTS (SELECT 1 FROM sibling) THEN 'cancelled' ELSE 'pending' END,
    run_at     = CASE WHEN EXISTS (SELECT 1 FROM sibling) THEN j.run_at ELSE $2::timestamptz END,
    last_error = CASE WHEN EXISTS (SELECT 1 FROM sibling) THEN $3::text || ' (superseded by the job already pending)' ELSE $3::text END,
    attempts   = j.attempts + $4::int,
    locked_at  = NULL,
    locked_by  = NULL,
    updated_at = now()
FROM me
WHERE j.id = me.id
RETURNING j.status
`, id, runAt, reason, addAttempts, lockedBy, staleFor.Seconds()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

// reapExpired takes back every job whose worker stopped renewing its lease:
// to pending (or cancelled, if a pending job already covers it), or to failed
// once it has used its attempts. Returns how many jobs it took back.
func (w *Worker) reapExpired(ctx context.Context) (int, error) {
	tm := w.t()
	rows, err := w.pool.Query(ctx, `
SELECT id, attempts, COALESCE(locked_by, '') FROM sync_jobs
WHERE status = 'running' AND COALESCE(locked_at, updated_at) < now() - make_interval(secs => $1)
ORDER BY locked_at
LIMIT 500
`, tm.leaseTimeout.Seconds())
	if err != nil {
		return 0, err
	}
	type expired struct {
		id       uuid.UUID
		attempts int
		lockedBy string
	}
	var list []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.attempts, &e.lockedBy); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, e := range list {
		reason := fmt.Sprintf("lease expired: worker %s stopped heartbeating", e.lockedBy)
		if e.attempts+1 >= maxAttempts {
			tag, err := w.pool.Exec(ctx, `
UPDATE sync_jobs
SET status = 'failed', attempts = attempts + 1, last_error = $2::text || ', giving up after ' || (attempts + 1) || ' attempts',
    locked_at = NULL, locked_by = NULL, updated_at = now()
WHERE id = $1 AND status = 'running' AND COALESCE(locked_at, updated_at) < now() - make_interval(secs => $3)
`, e.id, reason, tm.leaseTimeout.Seconds())
			if err != nil {
				return n, err
			}
			n += int(tag.RowsAffected())
			continue
		}
		st, err := w.requeue(ctx, e.id, time.Now(), reason, 1, "", tm.leaseTimeout)
		if err != nil {
			return n, err
		}
		if st != "" {
			n++
		}
	}
	if n > 0 {
		syncqueue.Wake()
	}
	return n, nil
}
