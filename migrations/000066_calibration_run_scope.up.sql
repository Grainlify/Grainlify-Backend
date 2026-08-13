-- Which rows a run scored.
--
-- Without it, re-reporting picks the most recent run per model and cannot tell
-- a main-scope run from a held-back one - so asking for one silently printed
-- the other's figures. Found by asking for both in sequence and getting
-- identical numbers for sets of 21 and 7.
ALTER TABLE calibration_model_runs ADD COLUMN IF NOT EXISTS scope TEXT;
