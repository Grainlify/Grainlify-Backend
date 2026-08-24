-- Restores the previous, narrower predicate.
--
-- Going back re-opens the defect this migration closed: an issue whose
-- assignment is 'completed' becomes eligible for a second draw again. Recorded
-- here rather than left for someone to rediscover from the diff.
DROP INDEX IF EXISTS idx_hackathon_assignments_one_unreleased;

CREATE UNIQUE INDEX IF NOT EXISTS idx_hackathon_assignments_one_active
  ON hackathon_assignments(hackathon_issue_id)
  WHERE status IN ('active', 'pr_submitted');

DROP FUNCTION IF EXISTS hackathon_assignment_released(TEXT);
