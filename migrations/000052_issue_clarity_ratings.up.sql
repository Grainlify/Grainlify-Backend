-- AI-specs.md §7's fourth safe maintainer criterion: "Contributor rating of
-- issue clarity, collected at PR submission (before payout is known)".
--
-- The parenthetical is the whole point. A rating collected after someone
-- knows what they were paid measures satisfaction with the payout, not the
-- clarity of the issue - and it is trivially gameable in both directions. So
-- the submission time is stored and checked at scoring time rather than
-- assumed: anything submitted at or after results were published is excluded
-- from a maintainer's score, not merely distrusted.
CREATE TABLE IF NOT EXISTS hackathon_issue_clarity_ratings (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  assignment_id UUID NOT NULL REFERENCES hackathon_assignments(id) ON DELETE CASCADE,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  hackathon_issue_id UUID REFERENCES hackathon_issues(id) ON DELETE SET NULL,
  issue_number INT NOT NULL,
  -- The contributor doing the rating. Never exposed to the maintainer: see
  -- the aggregate-only rule in ClarityAggregate.
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

  rating INT NOT NULL CHECK (rating BETWEEN 1 AND 5),
  -- Optional. A rating with no words is still signal; demanding an
  -- explanation is another reason to skip.
  comment TEXT,

  -- Load-bearing, not bookkeeping. Compared against
  -- hackathons.results_published_at when scoring.
  submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- One rating per contributor per assignment. Rating is optional
  -- throughout - a skipped rating is simply the absence of a row, so nothing
  -- here can make submitting a PR conditional on leaving one.
  UNIQUE (assignment_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_clarity_ratings_project
  ON hackathon_issue_clarity_ratings(project_id, hackathon_id);
CREATE INDEX IF NOT EXISTS idx_clarity_ratings_hackathon
  ON hackathon_issue_clarity_ratings(hackathon_id);
