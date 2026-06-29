DROP INDEX IF EXISTS idx_projects_ecosystem_id;

ALTER TABLE projects
  DROP COLUMN IF EXISTS ecosystem_id;
