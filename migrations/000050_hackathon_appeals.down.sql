ALTER TABLE hackathon_payout_runs DROP COLUMN IF EXISTS supersedes_run_id;
ALTER TABLE hackathon_payout_runs DROP COLUMN IF EXISTS trigger;

DROP TABLE IF EXISTS hackathon_appeals;

ALTER TABLE hackathons DROP COLUMN IF EXISTS appeals_closed_at;
ALTER TABLE hackathons DROP COLUMN IF EXISTS results_published_at;

-- Any hackathon sitting in one of the two new phases would violate the
-- narrowed constraint, so wind those rows back to the last phase the old
-- constraint allowed before reinstating it.
UPDATE hackathons SET phase = 'closed'
  WHERE phase IN ('results_published', 'settled');

ALTER TABLE hackathons DROP CONSTRAINT IF EXISTS hackathons_phase_check;
ALTER TABLE hackathons ADD CONSTRAINT hackathons_phase_check
  CHECK (phase IN (
    'draft',
    'application_period',
    'issue_prep',
    'live',
    'closed'
  ));
