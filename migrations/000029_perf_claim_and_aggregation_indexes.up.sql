-- Migration 000029: Performance indexes for sync_jobs claim query and
-- leaderboard/profile aggregation queries.
--
-- Problem 1 – sync_jobs claim query (runs every second per worker):
--   SELECT ... FROM sync_jobs WHERE status = 'pending' AND run_at <= now()
--   ORDER BY run_at ASC FOR UPDATE SKIP LOCKED LIMIT 1
--
--   Migration 000003 created idx_sync_jobs_pending ON sync_jobs(status, run_at).
--   To ensure the query uses the composite index correctly and avoid redundancy/duplication,
--   we drop the original idx_sync_jobs_pending and create the dedicated composite index
--   idx_sync_jobs_status_run_at on sync_jobs(status, run_at).
--
-- Problem 2 – leaderboard & profile queries use LOWER(author_login):
--   WHERE LOWER(i.author_login) = LOWER(ac.login)
--   WHERE LOWER(pr.author_login) = LOWER(ac.login)
--   WHERE LOWER(ga.login) = LOWER(ac.login)
--
--   The existing btree indexes on author_login (000014) are case-sensitive and
--   cannot satisfy a predicate on LOWER(author_login).  Functional indexes on
--   LOWER(author_login) / LOWER(login) allow PostgreSQL to execute these
--   correlated sub-selects as index scans instead of sequential scans.

-- ── sync_jobs ────────────────────────────────────────────────────────────────

-- Drop the broad index from 000003 that is now superseded.
-- The partial index below covers all queries that hit pending rows; the
-- idx_sync_jobs_project index in 000003 still covers project-based lookups.
DROP INDEX IF EXISTS idx_sync_jobs_pending;

-- Composite index on (status, run_at) for efficient worker claim queries.
-- Supports WHERE status = 'pending' AND run_at <= now() ORDER BY run_at ASC.
CREATE INDEX IF NOT EXISTS idx_sync_jobs_status_run_at
    ON sync_jobs (status, run_at);

-- ── leaderboard / profile – functional LOWER() indexes ───────────────────────

-- Functional index on LOWER(author_login) for github_issues.
-- Covers the correlated sub-select in leaderboard (LOWER(i.author_login) = ?)
-- and the profile query (i.author_login = $1 with case-insensitive join path).
CREATE INDEX IF NOT EXISTS idx_github_issues_author_login_lower
    ON github_issues (LOWER(author_login))
    WHERE author_login IS NOT NULL;

-- Functional index on LOWER(author_login) for github_pull_requests.
CREATE INDEX IF NOT EXISTS idx_github_prs_author_login_lower
    ON github_pull_requests (LOWER(author_login))
    WHERE author_login IS NOT NULL;

-- Functional index on LOWER(login) for github_accounts.
-- Covers the leaderboard LEFT JOIN: LOWER(ga.login) = LOWER(ac.login).
CREATE INDEX IF NOT EXISTS idx_github_accounts_login_lower
    ON github_accounts (LOWER(login))
    WHERE login IS NOT NULL;
