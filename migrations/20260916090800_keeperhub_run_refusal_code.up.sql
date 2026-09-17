-- #553's refusal gets a code of its own.
--
-- Migration 20260916090300 raised the generic integrity_constraint_violation,
-- and the KeeperHub rail recognised it by matching words in the message. This
-- replaces the trigger's function in place so it raises:
--
--   ERRCODE     KH002 - distinct from KH001, the refusal in the other direction
--               (20260916090700), so the two can never be confused
--   CONSTRAINT  keeperhub_payout_runs_one_rail_per_event_pool
--   DETAIL      which settlement the event already has
--   HINT        why the rule exists
--
-- The predicate is unchanged: a settlements row for the same (hackathon_id,
-- pool) refuses the run.
--
-- # Why a new migration rather than editing 20260916090300
--
-- golang-migrate never re-runs a version it has applied. An edit to 090300
-- would silently not reach any database already past it, which would then keep
-- raising the old code while the Go side only recognises the new one - and the
-- refusal would surface as a raw "insert run" failure.
--
-- The trigger itself is not recreated: it calls the function by name, so
-- replacing the function is enough.
--
-- MERGE ORDER. Part of a sequence that must be merged in order and deployed
-- once, after the last: #549, #550, #551, #552, #553, #554, #555, #556, #557,
-- #558, then this. golang-migrate applies only versions newer than the
-- database's current one, so a database deployed partway through silently skips
-- any lower-numbered migration merged later - observed on a test database that
-- went from 20260916090500 to 20260916090600 without ever applying
-- 20260916090100.

CREATE OR REPLACE FUNCTION keeperhub_refuse_run_when_already_settled()
RETURNS TRIGGER AS $$
DECLARE
  settlement_id UUID;
BEGIN
  SELECT id INTO settlement_id
  FROM settlements
  WHERE hackathon_id = NEW.hackathon_id
    AND pool = NEW.pool
  LIMIT 1;

  IF settlement_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % is already settled on the Aptos rail; a KeeperHub run cannot be opened for it',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'KH002',
            CONSTRAINT = 'keeperhub_payout_runs_one_rail_per_event_pool',
            DETAIL = format('settlements %s', settlement_id),
            HINT = 'An event is paid on one rail only. Opening this run would pay the same people a second time.';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
