-- One definition of "this assignment still holds its issue", in SQL, so the
-- index predicate and every query read the same rule.
--
-- WHY THE SMALL SET IS THE ONE ENUMERATED. The old rule was written the other
-- way round - status IN ('active','pr_submitted') - which enumerates the states
-- that OCCUPY an issue. That list went stale the moment 'completed' was added:
-- a merged assignment matched neither branch, so the issue read as free, was
-- re-advertised, and could be drawn to a second contributor.
--
-- Enumerating the released states inverts the default. A status nobody thought
-- about here is now treated as occupying the issue, which is the cautious
-- side: the failure becomes "an issue was not re-advertised when it could have
-- been", noticed by a person, instead of "finished work was handed to someone
-- else", noticed by two contributors and a payout.
--
-- IMMUTABLE because a partial index predicate requires it. The function is
-- pure text comparison over a fixed list, so this is honest rather than
-- convenient: it depends on nothing but its argument.
CREATE OR REPLACE FUNCTION hackathon_assignment_released(status TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
  SELECT status IN ('released_stale', 'released_voluntary', 'released_event_end')
$$;

COMMENT ON FUNCTION hackathon_assignment_released(TEXT) IS
  'True when an assignment has been released and its issue is free to re-draw. '
  'The complement - NOT released - means the issue is spoken for, INCLUDING completed. '
  'Enumerated this way round on purpose: a newly added status defaults to occupying the issue.';

-- Refuse loudly, and name what was found.
--
-- The index below cannot be created if any issue already carries two
-- unreleased assignments, and a bare unique-violation would report a duplicate
-- key without saying which issues or how this happened. That message would
-- reach somebody mid-deploy who has never read this file.
DO $$
DECLARE
  offenders INT;
  sample TEXT;
BEGIN
  SELECT count(*), string_agg(t.hackathon_issue_id::text || ' (' || t.n || ' assignments)', ', ')
    INTO offenders, sample
  FROM (
    SELECT hackathon_issue_id, count(*) AS n
    FROM hackathon_assignments
    WHERE NOT hackathon_assignment_released(status)
    GROUP BY hackathon_issue_id
    HAVING count(*) > 1
    ORDER BY count(*) DESC
    LIMIT 20
  ) t;

  IF COALESCE(offenders, 0) > 0 THEN
    RAISE EXCEPTION
      'cannot enforce one unreleased assignment per issue: % issue(s) already have more than one',
      offenders
      USING DETAIL = 'Offending issues: ' || COALESCE(sample, '(none listed)'),
            HINT = 'Each of these was drawn more than once - most likely an issue re-advertised after its '
                   'assignment completed, which is the defect this migration exists to prevent. Decide per issue '
                   'which assignment stands, release the others (released_voluntary), then re-run.';
  END IF;
END $$;

-- Replace the predicate rather than edit it: an index predicate cannot be
-- altered in place, so this is a drop and a create, and the old name goes with
-- it so a half-applied state is impossible to mistake for the new one.
DROP INDEX IF EXISTS idx_hackathon_assignments_one_active;

CREATE UNIQUE INDEX IF NOT EXISTS idx_hackathon_assignments_one_unreleased
  ON hackathon_assignments(hackathon_issue_id)
  WHERE NOT hackathon_assignment_released(status);

COMMENT ON INDEX idx_hackathon_assignments_one_unreleased IS
  'At most one unreleased assignment per issue. Replaces idx_hackathon_assignments_one_active, '
  'whose predicate listed active and pr_submitted and therefore let a COMPLETED issue be drawn again.';
