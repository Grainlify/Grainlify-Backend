-- Whether a project's repository is a fork of somebody else's.
--
-- A fork is a repo the contributor controls. They can open a pull request and
-- merge it themselves, with no review by anyone. Ranking counts merged pull
-- requests precisely because a merge is somebody else accepting the work - so
-- counting merges inside a fork turns the one number the platform claims is
-- unfarmable into a number anyone can mint at will: install the GitHub App on
-- all repositories, fork anything, merge into it.
--
-- This is not hypothetical arithmetic. The App installation grants whatever
-- the user did not deselect - "All repositories" is GitHub's default - and
-- syncInstallationRepositories creates AND auto-verifies a project for every
-- repo it can see. One installation in production carries 30 projects of
-- which 26 are forks of other organisations' repositories.
--
-- NULL means "not yet determined", not "not a fork". The distinction matters
-- during the backfill: a row we have not asked GitHub about is unknown, and
-- the ranking predicate must not silently treat unknown as safe. Every write
-- path sets this at creation, and 000066 ships with the backfill that resolves
-- every existing row, so NULL should not outlive the deploy.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS is_fork BOOLEAN;

-- Records where the answer came from, so a wrong value can be traced to the
-- thing that wrote it rather than guessed at.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS fork_checked_at TIMESTAMPTZ;

-- Ranking reads this on every leaderboard query, filtered to live verified
-- projects. Partial, because that is the only shape it is ever read in.
CREATE INDEX IF NOT EXISTS idx_projects_is_fork_live
  ON projects (is_fork)
  WHERE deleted_at IS NULL AND status = 'verified';
