-- An issue entered into a GrainHack via the GitHub label (AI-specs.md §2.2).
-- References (project_id, issue_number) the same way issue_applications
-- already does - not a FK to a github_issues surrogate key.
CREATE TABLE IF NOT EXISTS hackathon_issues (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_number INT NOT NULL,
  org_login TEXT NOT NULL, -- denormalized SPLIT_PART(github_full_name,'/',1) at insert time, for a fast per-org-cap query
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'published', 'removed')),
  acceptance_criteria TEXT,
  difficulty_tier TEXT CHECK (difficulty_tier IS NULL OR difficulty_tier IN ('easy', 'standard', 'advanced')),
  primary_language TEXT,
  -- False once a maintainer edits this manually, so a later re-sync never
  -- clobbers their edit with a fresh auto-detection.
  primary_language_auto_detected BOOLEAN NOT NULL DEFAULT true,
  -- Newcomer reservation (§3.8) and application window (§3.7) are inert in
  -- this slice - no logic reads these yet - but the spec's own §10.1 data
  -- model ties them to this table, so they're added now to avoid a
  -- near-future migration.
  reserved BOOLEAN,
  application_window_opens_at TIMESTAMPTZ,
  application_window_closes_at TIMESTAMPTZ,
  flagged_for_admin BOOLEAN NOT NULL DEFAULT false,
  flagged_reason TEXT,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at TIMESTAMPTZ,
  removed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, project_id, issue_number)
);
CREATE INDEX IF NOT EXISTS idx_hackathon_issues_org_cap ON hackathon_issues(hackathon_id, org_login) WHERE status != 'removed';
CREATE INDEX IF NOT EXISTS idx_hackathon_issues_status ON hackathon_issues(hackathon_id, status);
CREATE INDEX IF NOT EXISTS idx_hackathon_issues_project_issue ON hackathon_issues(project_id, issue_number);
