-- A project's application to participate in one GrainHack (AI-specs.md
-- §2.1). One row per (hackathon, project) - an org entering several repos
-- submits several rows, independently reviewable, since nothing in this
-- codebase groups projects by org as a first-class entity.
CREATE TABLE IF NOT EXISTS hackathon_project_applications (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  applicant_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  short_description TEXT NOT NULL,
  goal TEXT NOT NULL, -- "what they want from the event"
  expected_issue_count INT NOT NULL CHECK (expected_issue_count >= 0),
  maintainer_contact TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'accepted', 'rejected', 'more_info_requested')),
  reviewer_id UUID REFERENCES users(id) ON DELETE SET NULL,
  review_reason TEXT, -- required on reject/more_info_requested, shown to the applicant
  reviewed_at TIMESTAMPTZ,
  -- §2.1 auto-collected signals, computed lazily on first admin review (not
  -- eagerly for the whole queue) and cached here; refreshed if stale.
  signals JSONB,
  signals_computed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, project_id)
);
CREATE INDEX IF NOT EXISTS idx_hpa_hackathon_status ON hackathon_project_applications(hackathon_id, status);
CREATE INDEX IF NOT EXISTS idx_hpa_project ON hackathon_project_applications(project_id);
CREATE INDEX IF NOT EXISTS idx_hpa_applicant ON hackathon_project_applications(applicant_user_id);
