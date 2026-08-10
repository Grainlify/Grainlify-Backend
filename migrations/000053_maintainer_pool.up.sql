-- AI-specs.md §7, the maintainer pool.
--
-- Separate from every contributor payout table on purpose. §7 opens with the
-- reason: "Maintainers must never draw from the contributor pool. If they
-- share a pot, and maintainers also control which PRs merge, a maintainer can
-- create issues, merge friendly PRs, and drain the pool."
CREATE TABLE IF NOT EXISTS hackathon_maintainer_payouts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  org_login TEXT NOT NULL,
  maintainer_user_id UUID REFERENCES users(id) ON DELETE SET NULL,

  score NUMERIC(10,6) NOT NULL DEFAULT 0,

  -- Every criterion's measured value, its normalised form, the weight
  -- actually applied after renormalisation, and - when it was dropped - why.
  --
  -- Snapshotted because these inputs drift. Contributor counts change as data
  -- syncs, clarity aggregates change as late ratings land, and the review
  -- median is resampled from the API every time it is asked. If a score is
  -- recomputed during an appeal and comes back different, the appeal stops
  -- being about the decision and becomes about the arithmetic.
  criteria JSONB NOT NULL DEFAULT '[]'::jsonb,

  gross_amount NUMERIC(18,6) NOT NULL DEFAULT 0,
  holdback_pct INT NOT NULL DEFAULT 30,
  holdback_amount NUMERIC(18,6) NOT NULL DEFAULT 0,
  immediate_amount NUMERIC(18,6) NOT NULL DEFAULT 0,

  -- §7's holdback is the anti-farming mechanism, and it only works if the
  -- release is conditional. Releasing on a timer alone means a farmer waits
  -- out the clock and gets paid, which is the exact behaviour it exists to
  -- catch.
  holdback_due_at TIMESTAMPTZ,
  holdback_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (holdback_status IN ('pending', 'released', 'partially_released', 'withheld')),
  -- The evidence the release decision was made on, stored so it is auditable
  -- later rather than recomputed from a repo that has moved on.
  activity JSONB,
  activity_checked_at TIMESTAMPTZ,
  released_amount NUMERIC(18,6) NOT NULL DEFAULT 0,
  withheld_amount NUMERIC(18,6) NOT NULL DEFAULT 0,
  -- Where withheld money goes. An explicit decision rather than an accident
  -- of nobody having written it down.
  withheld_destination TEXT,
  holdback_resolved_at TIMESTAMPTZ,
  holdback_reason TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, project_id)
);

CREATE INDEX IF NOT EXISTS idx_maintainer_payouts_hackathon
  ON hackathon_maintainer_payouts(hackathon_id);
CREATE INDEX IF NOT EXISTS idx_maintainer_payouts_due
  ON hackathon_maintainer_payouts(holdback_due_at) WHERE holdback_status = 'pending';
