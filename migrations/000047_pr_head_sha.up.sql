-- The PR's head commit. CI runs against the head, not the merge commit, so
-- §5.1's "CI failing at merge" check needs this specific SHA - checks on the
-- merge commit are a different (often empty) set.
ALTER TABLE github_pull_requests ADD COLUMN IF NOT EXISTS head_sha TEXT;
