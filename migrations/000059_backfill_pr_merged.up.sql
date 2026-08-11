-- Backfill github_pull_requests.merged from the merge timestamp.
--
-- The repo-list sync path wrote `merged` from GitHub's "list pull requests"
-- response, which has no `merged` field - only `merged_at`. It unmarshalled
-- to false on every row. Before this migration production held 1299 pull
-- requests with a merge timestamp and 0 with merged = true.
--
-- The sync bug itself is fixed in internal/syncjobs/worker.go; this repairs
-- the rows already written. internal/ranking deliberately does not depend on
-- this having run - it accepts (merged OR merged_at_github IS NOT NULL) - so
-- the leaderboard is correct either way. This exists so the column stops
-- being a trap for the next reader who assumes it means what it says.
--
-- closed_at_github is not treated as a merge signal: a closed-unmerged pull
-- request has one too. Only an actual merge timestamp counts.
UPDATE github_pull_requests
SET merged = TRUE
WHERE merged = FALSE
  AND merged_at_github IS NOT NULL;

-- A merged pull request is closed. Rows carrying a merge timestamp while
-- still marked open are a sync artefact of the same bug, not real state.
UPDATE github_pull_requests
SET state = 'closed'
WHERE state = 'open'
  AND merged_at_github IS NOT NULL;
