-- Social follow becomes an eligibility gate for the Founding Contributor
-- Pool, on two platforms, submitted atomically.
--
-- Three changes, all structural rather than cosmetic:
--
--   1. Following is no longer a payment. social_follow_completions existed
--      only to record points_awarded, and with the points programme retired
--      that column would record zero forever - a column whose name is a lie.
--      Eligibility is now read from the submission's own status.
--   2. Two platforms, LinkedIn and X, replacing GitHub, Telegram and
--      LinkedIn.
--   3. One submission covering both platforms rather than one row per
--      (user, platform). Per-platform rows made a half-approved state
--      representable, and anything representable eventually happens.
--
-- Safe to drop rather than migrate: production held zero submissions and zero
-- completions across every platform and status when this was written, so
-- there is no data to preserve and no user to notify. Verified before writing
-- this, not assumed.

DROP TABLE IF EXISTS social_follow_completions;
DROP TABLE IF EXISTS social_follow_submissions;

CREATE TABLE IF NOT EXISTS social_follow_submissions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  -- One live submission per contributor. Resubmission after a rejection
  -- updates this row; the decision history lives in the log below, so
  -- overwriting the current state loses nothing.
  user_id UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,

  -- Both screenshots are NOT NULL, which is what makes the submission atomic
  -- at the schema level rather than only in the handler: a row carrying one
  -- platform's proof cannot exist, so a half-submitted state is not
  -- representable rather than merely discouraged.
  linkedin_screenshot TEXT NOT NULL,
  x_screenshot TEXT NOT NULL,

  -- 'revoked' is a distinct terminal state, not a return to 'rejected'.
  -- They mean different things to the person on the other end: rejected is
  -- "this proof was not good enough", revoked is "this was accepted and has
  -- since been withdrawn", and only the second needs explaining.
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'approved', 'rejected', 'revoked')),

  -- Who decided the CURRENT status, when, and why. Denormalised from the
  -- decision log so the common read - "what is this person's state and who
  -- put them there" - is one row rather than a join plus an ordering.
  decided_by UUID REFERENCES users(id) ON DELETE SET NULL,
  decided_at TIMESTAMPTZ,
  decision_reason TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_follow_submissions_status
  ON social_follow_submissions(status);

-- Append-only history of every decision taken on a submission.
--
-- Revoking is a status change and must never delete the submission or its
-- screenshots - the record of what was approved is exactly what a revocation
-- dispute turns on. This table is why a resubmission can safely overwrite the
-- current status above: the approval that was later revoked is still here,
-- with who did it and why.
CREATE TABLE IF NOT EXISTS social_follow_decisions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  submission_id UUID NOT NULL REFERENCES social_follow_submissions(id) ON DELETE CASCADE,
  decision TEXT NOT NULL
    CHECK (decision IN ('submitted', 'approved', 'rejected', 'revoked')),
  -- Required on a rejection or a revocation by the handler, not by the
  -- schema: 'submitted' legitimately has none.
  reason TEXT,
  -- Null for 'submitted', which the contributor does themselves.
  actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_social_follow_decisions_submission
  ON social_follow_decisions(submission_id, created_at DESC);
