-- Functional indexes for case-insensitive login lookups.
--
-- Seven non-test files match contributors with LOWER(author_login) = LOWER($1)
-- (leaderboard, user_profile, search, org_ratings, issue_applications,
-- hackathon/fit, hackathon/association), and four do the same against
-- github_accounts.login. The existing btree(author_login) / btree(login)
-- indexes cannot serve those predicates - a functional index is required for
-- Postgres to use one at all, so today every such lookup is a sequential scan.
--
-- Not CONCURRENTLY: golang-migrate runs each migration inside a transaction
-- and CREATE INDEX CONCURRENTLY cannot run in one. These tables are small
-- (~1.7k rows in production at the time of writing) so the build is
-- effectively instant and the brief ACCESS EXCLUSIVE lock is not a concern.
-- If these tables grow by orders of magnitude, a future index should be
-- created out-of-band rather than by widening this migration.

CREATE INDEX IF NOT EXISTS idx_github_issues_author_login_lower
    ON github_issues (LOWER(author_login))
    WHERE author_login IS NOT NULL AND author_login <> '';

CREATE INDEX IF NOT EXISTS idx_github_prs_author_login_lower
    ON github_pull_requests (LOWER(author_login))
    WHERE author_login IS NOT NULL AND author_login <> '';

CREATE INDEX IF NOT EXISTS idx_github_accounts_login_lower
    ON github_accounts (LOWER(login));
