-- Allow sync_jobs.status to hold the terminal 'dead' value for dead-lettered jobs.
-- The last_error column is added for observability (stores sanitized error text).
ALTER TABLE sync_jobs
    ADD COLUMN IF NOT EXISTS last_error TEXT;

-- Extend the status check constraint to include 'dead'.
-- We drop and recreate because ALTER CONSTRAINT is not supported for check constraints.
ALTER TABLE sync_jobs
    DROP CONSTRAINT IF EXISTS sync_jobs_status_check;

ALTER TABLE sync_jobs
    ADD CONSTRAINT sync_jobs_status_check
        CHECK (status IN ('pending', 'running', 'completed', 'failed', 'dead'));
