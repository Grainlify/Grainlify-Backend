-- Destroys the record of which attempts carried which legs. Safe only while no
-- run has been dispatched: afterwards it removes the evidence a resume needs to
-- avoid paying a leg twice, without undoing any payment.
DROP TABLE IF EXISTS keeperhub_payout_exclusions;
ALTER TABLE keeperhub_payout_legs DROP CONSTRAINT IF EXISTS keeperhub_payout_legs_confirmed_has_tx;
ALTER TABLE keeperhub_payout_legs DROP COLUMN IF EXISTS resolution_note;
ALTER TABLE keeperhub_payout_legs DROP COLUMN IF EXISTS resolved_by;
ALTER TABLE keeperhub_payout_legs DROP COLUMN IF EXISTS last_attempt_id;
DROP TABLE IF EXISTS keeperhub_dispatch_attempt_legs;
DROP TABLE IF EXISTS keeperhub_dispatch_attempts;
ALTER TABLE keeperhub_payout_runs DROP COLUMN IF EXISTS released_by;
ALTER TABLE keeperhub_payout_runs DROP COLUMN IF EXISTS hackathon_payout_run_id;
