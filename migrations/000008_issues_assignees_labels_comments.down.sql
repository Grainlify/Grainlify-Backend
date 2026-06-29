DROP INDEX IF EXISTS idx_github_issues_labels;

ALTER TABLE github_issues
  DROP COLUMN IF EXISTS comments_count,
  DROP COLUMN IF EXISTS labels,
  DROP COLUMN IF EXISTS assignees;
