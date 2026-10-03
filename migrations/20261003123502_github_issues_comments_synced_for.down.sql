-- Reverse of 20261003123502.
--
-- Rolling back LOCALLY means dropping the database, not running this by
-- hand: hand-editing schema_migrations leaves the recorded version
-- disagreeing with the files. See docs/RUNBOOK-ci.md.

ALTER TABLE github_issues DROP COLUMN IF EXISTS comments_synced_for;
