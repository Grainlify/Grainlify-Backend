-- Tracks a contributor's application to a specific issue as a first-class,
-- queryable entity. Previously "applying" only posted a GitHub comment, with
-- no structured record of who applied to what or its current status - see
-- internal/handlers/issue_applications_test.go's file-level doc comment for
-- the prior state. Written by the existing Apply/Assign/Reject/Withdraw/
-- Unassign handlers (internal/handlers/issue_applications.go) alongside
-- their existing GitHub API calls and github_issues JSONB mirror writes.
CREATE TABLE IF NOT EXISTS issue_applications (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_number INT NOT NULL,
  github_login TEXT NOT NULL,
  github_comment_id BIGINT,
  status TEXT NOT NULL DEFAULT 'applied', -- 'applied' | 'assigned' | 'rejected' | 'withdrawn'
  applied_at TIMESTAMPTZ,   -- set when status becomes 'applied'; NULL if this row started from a direct Assign() with no prior Apply()
  assigned_at TIMESTAMPTZ,  -- set when status becomes 'assigned'; cleared back to NULL on Unassign()'s revert-to-applied
  resolved_at TIMESTAMPTZ,  -- set when status becomes 'rejected' or 'withdrawn'
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, issue_number, user_id)
);
CREATE INDEX IF NOT EXISTS idx_issue_applications_user ON issue_applications(user_id);
CREATE INDEX IF NOT EXISTS idx_issue_applications_project_issue ON issue_applications(project_id, issue_number);

-- Supports the read-time PR<->issue regex lookup in
-- IssueApplicationsHandler.Mine() (matches GitHub closing keywords like
-- "Fixes #12" against pr.body) - no new column on github_pull_requests
-- itself, since the link is derived at query time rather than stored.
CREATE INDEX IF NOT EXISTS idx_github_pull_requests_project_author ON github_pull_requests(project_id, author_login);


