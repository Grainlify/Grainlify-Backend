-- Scope is required, not optional.
--
-- It was added nullable, which is exactly how it fails: some future path
-- forgets it, writes NULL, and -report goes back to guessing which run it is
-- looking at. That guess already happened once - a 7-row held-back run's
-- figures were printed when a 21-row main run was asked for - and a nullable
-- column is an invitation for it to happen again.
--
-- The CHECK closes the other half: a scope outside the known set would sort
-- into neither, and a run nobody can categorise is a run nobody can find.
UPDATE calibration_model_runs SET scope = 'main' WHERE scope IS NULL;

ALTER TABLE calibration_model_runs ALTER COLUMN scope SET NOT NULL;
ALTER TABLE calibration_model_runs
  ADD CONSTRAINT calibration_model_runs_scope_known
  CHECK (scope IN ('main', 'held-back'));
