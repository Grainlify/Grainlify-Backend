DROP INDEX IF EXISTS idx_projects_is_fork_live;
ALTER TABLE projects DROP COLUMN IF EXISTS fork_checked_at;
ALTER TABLE projects DROP COLUMN IF EXISTS is_fork;
