-- Legs first: they reference the run.
--
-- This drops the record of what was paid, so it is safe only while no run has
-- ever been dispatched - which is true today and stops being true the moment
-- one is. After that, dropping these tables does not undo the payments; it
-- only destroys the evidence of which legs already settled, leaving a re-run
-- with nothing to subtract and every reason to double-pay.
DROP INDEX IF EXISTS idx_keeperhub_payout_legs_unpaid;
DROP INDEX IF EXISTS idx_keeperhub_payout_legs_one_per_address;
DROP TABLE IF EXISTS keeperhub_payout_legs;
DROP TABLE IF EXISTS keeperhub_payout_runs;
