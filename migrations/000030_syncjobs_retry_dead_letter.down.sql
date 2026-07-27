-- Revert: remove 'dead' from status constraint and drop last_error column.
ALTER TABLE sync_jobs
    DROP CONSTRAINT IF EXISTS sync_jobs_status_check;

ALTER TABLE sync_jobs
    ADD CONSTRAINT sync_jobs_status_check
        CHECK (status IN ('pending', 'running', 'completed', 'failed'));

ALTER TABLE sync_jobs
    DROP COLUMN IF EXISTS last_error;
