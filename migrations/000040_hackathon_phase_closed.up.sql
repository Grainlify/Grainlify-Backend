-- Widen the phase enum with 'closed' (AI-specs.md §1 Phase 4). The
-- assignment pipeline (§4) has no terminal state without it: nothing closes
-- application windows for good, nothing releases in-flight assignments at
-- event end, and the reconciler would enqueue sync jobs forever.
--
-- Phases 5-6 (results_published, settled) belong to the judging/payout
-- slice and will widen this constraint again the same way.
ALTER TABLE hackathons DROP CONSTRAINT IF EXISTS hackathons_phase_check;
ALTER TABLE hackathons ADD CONSTRAINT hackathons_phase_check
  CHECK (phase IN ('draft', 'application_period', 'issue_prep', 'live', 'closed'));
