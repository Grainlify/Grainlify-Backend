ALTER TABLE hackathon_verdicts DROP COLUMN IF EXISTS effective_units;
ALTER TABLE hackathon_verdicts DROP COLUMN IF EXISTS curve_multiplier;
ALTER TABLE hackathon_verdicts DROP COLUMN IF EXISTS curve_position;
ALTER TABLE hackathon_payout_runs DROP COLUMN IF EXISTS curve;
ALTER TABLE hackathon_payout_runs DROP COLUMN IF EXISTS curve_applied;
ALTER TABLE hackathon_payout_runs ALTER COLUMN total_units TYPE INT USING ROUND(total_units);
