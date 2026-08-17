-- Who merged the pull request.
--
-- The platform's public claim is that rewards cannot be farmed, and the most
-- basic form of farming - authoring a pull request and merging it yourself -
-- was not weakly detected, it was UNDETECTABLE. github_pull_requests stored
-- author_login, merged and merged_at_github, and nothing about who accepted
-- the work.
--
-- Ranking counts merged pull requests precisely because a merge means somebody
-- else accepted the work. Without this column that premise was unverifiable
-- for every row in the table.
--
-- The only proxy available before this was "is the repo in the author's own
-- namespace", which found 7 merges by 1 account out of 3,195 - a floor, not a
-- measurement, because it misses anyone who creates an organisation first
-- (25 of 30 owners are orgs, and creating one takes a minute) and anyone who
-- is a collaborator on a repo they do not own.
--
-- NULL means "not recorded", not "nobody". Rows synced before this column
-- existed cannot be distinguished from rows GitHub genuinely reports no merger
-- for, and an audit must not read one as the other. Backfilling is possible
-- but costs one API call per pull request: merged_by is absent from the LIST
-- endpoint the sync uses and present only on the single-PR GET - the same trap
-- as `merged` itself.
ALTER TABLE github_pull_requests ADD COLUMN IF NOT EXISTS merged_by_login TEXT;

-- Partial: the audit queries only ever ask about merged rows.
CREATE INDEX IF NOT EXISTS idx_prs_merged_by_login
  ON github_pull_requests (merged_by_login)
  WHERE merged_by_login IS NOT NULL;
