-- Reverse of 20261003122120.
--
-- Rolling back LOCALLY means dropping the database, not running this by
-- hand: hand-editing schema_migrations leaves the recorded version
-- disagreeing with the files. See docs/RUNBOOK-ci.md.
--
-- Cancelled rows are not resurrected: they were duplicates of a job that ran.
-- They are folded into 'failed' only so the narrower CHECK can be restored.

DROP INDEX IF EXISTS uq_sync_jobs_one_pending;

UPDATE sync_jobs SET status = 'failed' WHERE status = 'cancelled';

ALTER TABLE sync_jobs DROP CONSTRAINT IF EXISTS sync_jobs_status_check;
ALTER TABLE sync_jobs ADD CONSTRAINT sync_jobs_status_check
  CHECK (status IN ('pending', 'running', 'completed', 'failed'));
