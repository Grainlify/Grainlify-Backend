-- Any hackathon already in 'closed' has no valid earlier phase to fall back
-- to, so move it to 'live' (the phase it came from) before narrowing the
-- constraint - otherwise the ALTER fails on existing rows.
UPDATE hackathons SET phase = 'live' WHERE phase = 'closed';
ALTER TABLE hackathons DROP CONSTRAINT IF EXISTS hackathons_phase_check;
ALTER TABLE hackathons ADD CONSTRAINT hackathons_phase_check
  CHECK (phase IN ('draft', 'application_period', 'issue_prep', 'live'));
