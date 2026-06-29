DROP INDEX IF EXISTS idx_projects_status;
DROP INDEX IF EXISTS idx_projects_tags;
DROP INDEX IF EXISTS idx_projects_category;
DROP INDEX IF EXISTS idx_projects_language;

ALTER TABLE projects
  DROP COLUMN IF EXISTS category,
  DROP COLUMN IF EXISTS tags,
  DROP COLUMN IF EXISTS language;
