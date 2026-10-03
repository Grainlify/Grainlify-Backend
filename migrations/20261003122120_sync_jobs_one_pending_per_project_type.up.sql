-- sync_jobs_one_pending_per_project_type
--
-- Every issues / pull_request / push / issue_comment webhook inserted a fresh
-- sync_issues + sync_prs pair, with no check for one already waiting. A single
-- worker cannot drain that: in production on 2026-10-03 there were 38,145
-- pending rows covering only 206 distinct (project, job type) pairs, the oldest
-- waiting since 2026-09-25. Each of those rows is a full re-read of the
-- repository, and the per-issue comment fetches they make are what exhausts
-- the project owner's GitHub rate limit (the 403s that fail ~half of all jobs).
--
-- A pending job reads the repository when it starts, so a second pending job
-- for the same pair can only ever see the same thing. This makes "at most one
-- pending job per (project, job type)" a constraint, so the enqueue paths can
-- use ON CONFLICT DO NOTHING and coalesce instead of piling up. A RUNNING job
-- does not count: a webhook that arrives while a sync is mid-flight may describe
-- a change the sync already read past, so one more pending job is still queued
-- behind it (the worker never starts a second job for a pair that is running).
--
-- Safe on production's current data, and against the old code still running
-- during the deploy overlap:
--
--   * The duplicates are collapsed first, inside this migration, so the unique
--     index cannot fail on them. They are marked 'cancelled' with a reason, not
--     deleted - the oldest pending row per pair is kept (it runs first anyway).
--   * The table is locked for the length of the migration. Without the lock,
--     the old instance's webhook handler could insert a new duplicate between
--     the collapse and CREATE UNIQUE INDEX, the index build would fail, and the
--     failed migration would leave schema_migrations dirty and block the deploy.
--     golang-migrate runs this file as one implicit transaction, so the lock is
--     held until the index exists. Measured at 123k rows / 36 MB, the collapse
--     touches ~38k rows; this is a few seconds of blocked sync_jobs writes.
--     Nothing user-facing waits on sync_jobs (webhook ingestion writes its
--     snapshot rows first and enqueues last).
--   * Not CONCURRENTLY: CREATE INDEX CONCURRENTLY cannot run inside the
--     transaction golang-migrate wraps each migration in (see 000049).
--   * After this lands, the old code's plain INSERT of a duplicate fails with a
--     unique violation. Every old call site discards that error (`_, _ =`), so
--     the overlap degrades to "the duplicate was not queued", which is the
--     point of the change.
--
-- scripts/sync_jobs_cleanup.sql does the same collapse (plus resetting jobs
-- stuck in 'running') and can be run before the deploy so this migration finds
-- nothing to collapse; it is not required for this migration to succeed.

LOCK TABLE sync_jobs IN ACCESS EXCLUSIVE MODE;

ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS sync_jobs_status_check;
ALTER TABLE sync_jobs ADD CONSTRAINT sync_jobs_status_check
  CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled'));

WITH ranked AS (
  SELECT id,
         row_number() OVER (PARTITION BY project_id, job_type ORDER BY run_at, created_at, id) AS rn
  FROM sync_jobs
  WHERE status = 'pending'
)
UPDATE sync_jobs j
SET status = 'cancelled',
    last_error = 'coalesced: another pending job for this project and job type already covers it (migration 20261003122120)',
    updated_at = now()
FROM ranked r
WHERE j.id = r.id AND r.rn > 1;

CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_jobs_one_pending
  ON sync_jobs (project_id, job_type)
  WHERE status = 'pending';
