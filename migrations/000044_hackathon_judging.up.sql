-- The AI-specs.md §5 judging pipeline. One row per PR being judged, with
-- every stage's output on it, because §5's opening line requires that
-- "every stage writes to the database so any stage can be re-run
-- independently".
CREATE TABLE IF NOT EXISTS hackathon_verdicts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  hackathon_issue_id UUID REFERENCES hackathon_issues(id) ON DELETE SET NULL,
  assignment_id UUID REFERENCES hackathon_assignments(id) ON DELETE SET NULL,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  pr_number INT NOT NULL,
  user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  github_login TEXT NOT NULL,

  -- §5.1 pre-filter. Deterministic; runs with no model call.
  prefilter_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (prefilter_status IN ('pending', 'passed', 'rejected')),
  prefilter_reason TEXT,
  -- §5.3 requires diff_stats be "computed in code, never estimated by the
  -- model" - that is what stops verbosity gaming, since nothing inside the
  -- diff can misrepresent its own size or generated-line ratio.
  diff_stats JSONB,

  -- §5.2 duplicate detection. Flags for human review, never auto-rejects:
  -- two people independently fixing the same obvious bug happens.
  duplicate_of_verdict_id UUID REFERENCES hackathon_verdicts(id) ON DELETE SET NULL,
  duplicate_similarity DOUBLE PRECISION,
  duplicate_flagged BOOLEAN NOT NULL DEFAULT false,

  -- §5.3-5.5 model verdicts. NULL until those stages run; the pipeline is
  -- fully exercisable without them.
  judge_bucket TEXT CHECK (judge_bucket IS NULL OR judge_bucket IN ('rejected','accepted','substantial','exceptional')),
  judge_confidence TEXT CHECK (judge_confidence IS NULL OR judge_confidence IN ('low','medium','high')),
  judge_payload JSONB,
  judge_model TEXT,
  cross_check_bucket TEXT CHECK (cross_check_bucket IS NULL OR cross_check_bucket IN ('rejected','accepted','substantial','exceptional')),
  cross_check_payload JSONB,
  cross_check_model TEXT,
  escalation_bucket TEXT CHECK (escalation_bucket IS NULL OR escalation_bucket IN ('rejected','accepted','substantial','exceptional')),
  escalation_payload JSONB,
  needs_human_review BOOLEAN NOT NULL DEFAULT false,
  review_reason TEXT,

  -- The bucket that actually counts. A human override always wins (§5.7).
  final_bucket TEXT CHECK (final_bucket IS NULL OR final_bucket IN ('rejected','accepted','substantial','exceptional')),
  final_source TEXT CHECK (final_source IS NULL OR final_source IN ('prefilter','auto_confirmed','escalation','human_override')),
  overridden_by UUID REFERENCES users(id) ON DELETE SET NULL,
  override_reason TEXT,
  overridden_at TIMESTAMPTZ,

  -- §5.8 payout. Units and amount are only written when a payout run
  -- happens, so a shadow-mode event computes verdicts and pays nothing.
  units INT,
  payout_amount NUMERIC(18,6),
  payout_run_id UUID,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, project_id, pr_number)
);
CREATE INDEX IF NOT EXISTS idx_hackathon_verdicts_hackathon ON hackathon_verdicts(hackathon_id, final_bucket);
CREATE INDEX IF NOT EXISTS idx_hackathon_verdicts_review ON hackathon_verdicts(hackathon_id) WHERE needs_human_review;
CREATE INDEX IF NOT EXISTS idx_hackathon_verdicts_user ON hackathon_verdicts(user_id);

-- §5.8 payout runs. Separate from the verdicts so a payout can be computed,
-- inspected, and recomputed without touching judging - and so shadow mode
-- (compute everything, publish nothing, pay by hand) is the default rather
-- than something you have to remember to do.
CREATE TABLE IF NOT EXISTS hackathon_payout_runs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  contributor_prize_pool NUMERIC(18,6) NOT NULL,
  total_units INT NOT NULL,
  unit_value NUMERIC(18,6) NOT NULL,
  -- True when unit_value fell below payout_floor and
  -- payout_floor_strategy had to fund the highest buckets first.
  floor_applied BOOLEAN NOT NULL DEFAULT false,
  payout_floor NUMERIC(18,6),
  funded_buckets JSONB,
  unfunded_count INT NOT NULL DEFAULT 0,
  -- A run is a calculation until someone explicitly publishes it.
  published BOOLEAN NOT NULL DEFAULT false,
  published_at TIMESTAMPTZ,
  computed_by UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_hackathon_payout_runs_hackathon ON hackathon_payout_runs(hackathon_id, created_at DESC);
