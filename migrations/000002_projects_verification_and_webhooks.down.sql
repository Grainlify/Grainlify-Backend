DROP TABLE IF EXISTS github_webhook_deliveries;

ALTER TABLE projects
  DROP COLUMN IF EXISTS webhook_created_at,
  DROP COLUMN IF EXISTS webhook_url,
  DROP COLUMN IF EXISTS verification_error,
  DROP COLUMN IF EXISTS verified_at,
  DROP COLUMN IF EXISTS github_repo_id;
