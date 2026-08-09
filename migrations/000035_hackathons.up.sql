-- GrainHack: a time-boxed hackathon run on the platform. This is Slice 1
-- ("Foundation") of the feature described in AI-specs.md - the lifecycle
-- mechanics for phases draft/application_period/issue_prep/live only.
-- Judging/results/settled phases, the AI assignment/judging pipelines, and
-- payouts are future slices and will widen the `phase` CHECK and add their
-- own tables rather than touching this one.
CREATE TABLE IF NOT EXISTS hackathons (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name TEXT NOT NULL,
  phase TEXT NOT NULL DEFAULT 'draft'
    CHECK (phase IN ('draft', 'application_period', 'issue_prep', 'live')),
  -- Load-bearing per spec §1.1: account-age/repo-history gates in later
  -- slices are measured against this, not created_at.
  announced_at TIMESTAMPTZ,
  application_period_start TIMESTAMPTZ,
  application_period_end TIMESTAMPTZ,
  issue_prep_start TIMESTAMPTZ,
  starts_at TIMESTAMPTZ,
  ends_at TIMESTAMPTZ,
  merge_grace_period_hours INT NOT NULL DEFAULT 48,
  contributor_prize_pool NUMERIC(18,6),
  maintainer_prize_pool NUMERIC(18,6),
  -- Written once, on the issue_prep -> live transition (internal/hackathon
  -- phase.go). From that point the running hackathon reads this snapshot,
  -- not live global config - see AI-specs.md §1.1.
  config_snapshot JSONB,
  config_snapshot_taken_at TIMESTAMPTZ,
  created_by UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_hackathons_phase ON hackathons(phase);
