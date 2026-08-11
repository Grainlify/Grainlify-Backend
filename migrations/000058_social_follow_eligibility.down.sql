-- Restores the per-platform shape from 000031. Screenshots submitted under
-- the two-platform model are not carried back: the old table has one row per
-- (user, platform) with a `platform` value this model no longer has, and
-- inventing 'github'/'telegram' rows for people who never submitted them
-- would be worse than an empty table.
DROP TABLE IF EXISTS social_follow_decisions;
DROP TABLE IF EXISTS social_follow_submissions;

CREATE TABLE IF NOT EXISTS social_follow_submissions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  platform TEXT NOT NULL,
  screenshot TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  rejection_reason TEXT,
  reviewed_by UUID REFERENCES users(id),
  reviewed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, platform)
);

CREATE TABLE IF NOT EXISTS social_follow_completions (
  user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  points_awarded INT NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
