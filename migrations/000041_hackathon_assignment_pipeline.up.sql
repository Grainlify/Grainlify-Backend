-- The AI-specs.md §4 assignment pipeline. Four tables:
--   hackathon_issue_applications - a contributor applying to one issue
--   hackathon_draws              - one weighted draw, replayable via its seed
--   hackathon_assignments        - the resulting assignment + its lifecycle
--   hackathon_contributor_profiles - cached GitHub snapshot for Layer 2
--
-- Note the name: hackathon_project_applications (§2.1) is a PROJECT applying
-- to a hackathon. This is a CONTRIBUTOR applying to an ISSUE. Different
-- grain, different table, deliberately distinct names.

-- §4.1/§4.3: one row per (issue, contributor). Gate failures are stored
-- rather than discarded so the applicant can be shown the specific reason
-- (§4.1) and so an appeal can see why someone never entered a pool.
CREATE TABLE IF NOT EXISTS hackathon_issue_applications (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  hackathon_issue_id UUID NOT NULL REFERENCES hackathon_issues(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  github_login TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'applied'
    CHECK (status IN ('applied', 'rejected_gate', 'won', 'lost', 'withdrawn')),
  -- The specific §4.1 gate that failed, shown verbatim to the applicant.
  gate_failure_reason TEXT,
  -- Untrusted free text (§4.4). Stored for the record and for Layer 2's
  -- <application_text>; never weighted by the draw.
  application_text TEXT,
  -- §4.3 Layer 2 output. NULL until assessed; when the AI flag is off the
  -- stub writes 'plausible' so the pipeline stays fully exercisable.
  fit TEXT CHECK (fit IS NULL OR fit IN ('strong', 'plausible', 'weak')),
  difficulty_match TEXT CHECK (difficulty_match IS NULL OR difficulty_match IN ('below', 'matched', 'above')),
  fit_evidence TEXT,
  fit_concerns JSONB NOT NULL DEFAULT '[]'::jsonb,
  fit_assessed_at TIMESTAMPTZ,
  fit_model TEXT,
  fit_prompt_version TEXT,
  -- §4.2: evidence, never a gate. Surfaced in admin review.
  prior_association JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_issue_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_hackathon_issue_apps_issue ON hackathon_issue_applications(hackathon_issue_id, status);
-- Backs the §13-#1 concurrent-open-application cap and the per-event
-- abandon/org-cap counts, all of which are per (hackathon, user).
CREATE INDEX IF NOT EXISTS idx_hackathon_issue_apps_user ON hackathon_issue_applications(hackathon_id, user_id, status);

-- §4.5.4: "Use a seeded RNG and store the seed on the draw record so any
-- draw can be replayed during an appeal." pool holds the full per-applicant
-- ticket breakdown, so a replay needs no other table.
CREATE TABLE IF NOT EXISTS hackathon_draws (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  hackathon_issue_id UUID NOT NULL REFERENCES hackathon_issues(id) ON DELETE CASCADE,
  seed BIGINT NOT NULL,
  -- [{user_id, github_login, fit, tickets, weights:{name:factor}}, ...]
  pool JSONB NOT NULL DEFAULT '[]'::jsonb,
  pool_size INT NOT NULL DEFAULT 0,
  winner_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  -- Which §4.5 fallbacks fired, so an unassigned or oddly-assigned issue is
  -- explainable after the fact rather than needing the code re-read.
  used_weak_pool BOOLEAN NOT NULL DEFAULT false,
  reservation_applied BOOLEAN NOT NULL DEFAULT false,
  reservation_fell_back BOOLEAN NOT NULL DEFAULT false,
  first_come_fallback BOOLEAN NOT NULL DEFAULT false,
  no_winner_reason TEXT,
  -- true = admin "simulate draw": full pipeline, real applicants, no writes
  -- to hackathon_assignments. Assignment cannot run in shadow mode, so this
  -- is how weights get sanity-checked against a real pool before an event.
  is_simulation BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_hackathon_draws_issue ON hackathon_draws(hackathon_issue_id, created_at DESC);

-- §4.6 assignment lifecycle.
CREATE TABLE IF NOT EXISTS hackathon_assignments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  hackathon_issue_id UUID NOT NULL REFERENCES hackathon_issues(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_number INT NOT NULL,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  github_login TEXT NOT NULL,
  org_login TEXT NOT NULL,
  draw_id UUID REFERENCES hackathon_draws(id) ON DELETE SET NULL,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'pr_submitted', 'completed', 'released_stale', 'released_voluntary', 'released_event_end')),
  -- Slot accounting is separate from status because slot_freed_on is
  -- configurable (§3.4): with 'pr_submission' the slot frees while the
  -- assignment is still open, so "holds a slot" is NOT "status = active".
  holds_slot BOOLEAN NOT NULL DEFAULT true,
  assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- assigned_at + stale_assignment_days, resolved at write time from the
  -- config in force then, so later config edits never retroactively move
  -- an existing assignment's deadline.
  stale_at TIMESTAMPTZ,
  qualifying_pr_number INT,
  qualifying_pr_at TIMESTAMPTZ,
  merged_at TIMESTAMPTZ,
  released_at TIMESTAMPTZ,
  release_reason TEXT,
  abandon_recorded BOOLEAN NOT NULL DEFAULT false,
  -- §13 #2: contributors are warned before ends_at, not after.
  end_of_event_notified_at TIMESTAMPTZ,
  prior_association JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One live assignment per issue; a released issue can be re-drawn, so this
-- is a partial index rather than a plain UNIQUE on hackathon_issue_id.
CREATE UNIQUE INDEX IF NOT EXISTS idx_hackathon_assignments_one_active
  ON hackathon_assignments(hackathon_issue_id)
  WHERE status IN ('active', 'pr_submitted');
CREATE INDEX IF NOT EXISTS idx_hackathon_assignments_user ON hackathon_assignments(hackathon_id, user_id, status);
CREATE INDEX IF NOT EXISTS idx_hackathon_assignments_slots ON hackathon_assignments(hackathon_id, user_id) WHERE holds_slot;
CREATE INDEX IF NOT EXISTS idx_hackathon_assignments_org_cap ON hackathon_assignments(hackathon_id, user_id, org_login);
CREATE INDEX IF NOT EXISTS idx_hackathon_assignments_stale ON hackathon_assignments(stale_at) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_hackathon_assignments_project_issue ON hackathon_assignments(project_id, issue_number);

-- §4.3: "cached GitHub profile snapshot, built on first application in the
-- event and reused for all later applications (refresh if older than 7
-- days). One crawl per person, not per application."
CREATE TABLE IF NOT EXISTS hackathon_contributor_profiles (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  github_login TEXT NOT NULL,
  snapshot JSONB NOT NULL,
  computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, user_id)
);
