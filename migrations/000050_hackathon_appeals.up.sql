-- AI-specs.md §6 (appeals) and the last two lifecycle phases from §1.
--
-- Phase 5 (results_published) opens the appeal window; phase 6 (settled) is
-- where contributor payouts release and the maintainer holdback timer starts.
-- Both are added here because the appeal window hangs off the transition into
-- phase 5 and the release gate hangs off the transition into phase 6 - the
-- appeal machinery below is meaningless without them.

ALTER TABLE hackathons DROP CONSTRAINT IF EXISTS hackathons_phase_check;
ALTER TABLE hackathons ADD CONSTRAINT hackathons_phase_check
  CHECK (phase IN (
    'draft',
    'application_period',
    'issue_prep',
    'live',
    'closed',
    'results_published',
    'settled'
  ));

-- When the appeal window opened. Set on the closed -> results_published
-- transition and never by a cron job: §1 is explicit that "each phase
-- transition is an explicit admin action, not a cron job firing on a date",
-- so the window is anchored to the moment a human published results, not to
-- a planned date that may have slipped.
ALTER TABLE hackathons ADD COLUMN IF NOT EXISTS results_published_at TIMESTAMPTZ;

-- When the appeal window was closed out and the §13-#4 recompute ran. Doubles
-- as the idempotency marker for that recompute: it must happen exactly once,
-- so a second attempt can see it has already been done rather than paying
-- everyone twice from a re-divided pool.
ALTER TABLE hackathons ADD COLUMN IF NOT EXISTS appeals_closed_at TIMESTAMPTZ;

-- §6: "Appeal routes to a human with both model verdicts and the diff. Human
-- decision is final and recorded."
CREATE TABLE IF NOT EXISTS hackathon_appeals (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  verdict_id UUID NOT NULL REFERENCES hackathon_verdicts(id) ON DELETE CASCADE,
  user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  github_login TEXT NOT NULL,

  -- The contributor's own words. Required: an appeal with no stated grounds
  -- gives the reviewer nothing to actually review.
  reason TEXT NOT NULL CHECK (length(btrim(reason)) > 0),

  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'upheld', 'rejected')),

  -- The human decision. decision_reason is required on any decision (enforced
  -- in Go, not here, so the message can explain *why* it is required) and
  -- feeds the §8 calibration set the same way an admin override does - a
  -- disagreement between a model and a human is exactly the data that set is
  -- short of.
  reviewer_id UUID REFERENCES users(id) ON DELETE SET NULL,
  decision_reason TEXT,
  decided_bucket TEXT
    CHECK (decided_bucket IS NULL OR decided_bucket IN ('rejected','accepted','substantial','exceptional')),
  decided_at TIMESTAMPTZ,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- One appeal per verdict. A contributor who disagrees again after a
  -- decision is asking for the decision to be reopened, which is an admin
  -- action, not a second appeal racing the first.
  UNIQUE (verdict_id)
);

CREATE INDEX IF NOT EXISTS idx_hackathon_appeals_hackathon
  ON hackathon_appeals(hackathon_id, status);
CREATE INDEX IF NOT EXISTS idx_hackathon_appeals_pending
  ON hackathon_appeals(hackathon_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_hackathon_appeals_user
  ON hackathon_appeals(user_id);

-- Why a payout run exists. §13-#4 requires a recompute after appeals resolve,
-- and a run produced by that recompute must be distinguishable from the
-- original - otherwise "which numbers were actually published?" is answered
-- by timestamp ordering and hope.
ALTER TABLE hackathon_payout_runs ADD COLUMN IF NOT EXISTS trigger TEXT
  NOT NULL DEFAULT 'initial'
  CHECK (trigger IN ('initial', 'appeal_recompute'));

-- The run this one replaces, when it is a recompute.
ALTER TABLE hackathon_payout_runs ADD COLUMN IF NOT EXISTS supersedes_run_id UUID
  REFERENCES hackathon_payout_runs(id) ON DELETE SET NULL;
