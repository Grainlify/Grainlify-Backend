DROP INDEX IF EXISTS idx_prs_merged_by_login;
ALTER TABLE github_pull_requests DROP COLUMN IF EXISTS merged_by_login;
