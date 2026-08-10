-- Diminishing returns per contributor (§5.8 extension).
--
-- total_units becomes fractional: a second PR at 0.8 of a 3-unit bucket
-- contributes 2.4, so an integer column would silently truncate the divisor
-- and misprice every payout in the run.
ALTER TABLE hackathon_payout_runs
  ALTER COLUMN total_units TYPE NUMERIC(18,6);

-- The curve settings this run used, snapshotted so a payout stays
-- reproducible from its own record after the config changes.
ALTER TABLE hackathon_payout_runs ADD COLUMN IF NOT EXISTS curve_applied BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE hackathon_payout_runs ADD COLUMN IF NOT EXISTS curve JSONB;

-- Per-PR curve detail. An appeal asks "why did this PR earn less than that
-- one?", and the answer is its position in the contributor's own ordering -
-- which cannot be reconstructed later once merge times or bucket decisions
-- have moved.
ALTER TABLE hackathon_verdicts ADD COLUMN IF NOT EXISTS curve_position INT;
ALTER TABLE hackathon_verdicts ADD COLUMN IF NOT EXISTS curve_multiplier NUMERIC(6,4);
ALTER TABLE hackathon_verdicts ADD COLUMN IF NOT EXISTS effective_units NUMERIC(12,6);
