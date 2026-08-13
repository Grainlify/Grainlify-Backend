ALTER TABLE calibration_model_runs DROP CONSTRAINT IF EXISTS calibration_model_runs_scope_known;
ALTER TABLE calibration_model_runs ALTER COLUMN scope DROP NOT NULL;
