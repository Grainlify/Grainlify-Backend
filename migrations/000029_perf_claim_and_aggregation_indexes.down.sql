-- Revert migration 000029: remove the performance indexes added for the
-- sync_jobs claim query and leaderboard/profile LOWER() aggregations,
-- then restore the original broad index from migration 000003.

DROP INDEX IF EXISTS idx_sync_jobs_status_run_at;
DROP INDEX IF EXISTS idx_github_issues_author_login_lower;
DROP INDEX IF EXISTS idx_github_prs_author_login_lower;
DROP INDEX IF EXISTS idx_github_accounts_login_lower;

-- Restore the original index that was dropped in the up migration.
CREATE INDEX IF NOT EXISTS idx_sync_jobs_pending ON sync_jobs (status, run_at);
