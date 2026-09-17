-- An event and pool is paid on Base or on Aptos, never both - now in BOTH
-- directions.
--
-- Migration 20260916090300 refused a KeeperHub run for an event the Aptos rail
-- had already settled. That left the other order open: an event with a KeeperHub
-- run could still be settled on Aptos, and both rails would pay the same people.
-- This is that missing half, on the Aptos side.
--
-- # The predicate: ANY KeeperHub run for the same (hackathon_id, pool)
--
-- Not "a run that has paid". A run that is only planned can still be
-- dispatched, so waiting for money to move would leave a window in which both
-- rails are live for one event. Symmetric with 090300, which refuses on the mere
-- existence of a settlements row.
--
-- Checked, before writing this: no legitimate flow inserts a settlement for an
-- event that has a KeeperHub run. settlement.Persist is the only writer, `payout
-- persist` its only caller, there is no re-persist or supersede path (one row
-- per event and pool is already a unique index), and the KeeperHub release path
-- never writes this table. KeeperHub runs have no cancelled or abandoned state -
-- only planned, dispatching, complete and failed - and a failed run may have
-- moved money, so it blocks too.
--
-- # Founding is untouched
--
-- Founding settlements carry no hackathon_id. The trigger returns immediately
-- for them, before any query.
--
-- # How the refusal reads
--
-- A custom SQLSTATE, KH001, in the implementation-defined class range and not a
-- class Postgres uses, so Go recognises it by code rather than by message text
-- and turns it into a policy sentence (internal/settlement/rail_exclusion.go).
-- A raw exception reaching an operator reads as a bug; this is a rule.
--
-- MERGE ORDER. Part of a sequence that must be merged in order and deployed
-- once, after the last: #549, #550, #551, #552, #553, #554, #555, #556, #557,
-- then this. golang-migrate applies only versions newer than the database's
-- current one, so a database deployed partway through silently skips any
-- lower-numbered migration merged afterwards. That was observed on a test
-- database: at 20260916090500, migrated forward, it reached 20260916090600 and
-- never applied 20260916090100. This version is 20260916090700 so that it sorts
-- after everything in the stack.

CREATE OR REPLACE FUNCTION settlements_refuse_event_paid_on_keeperhub()
RETURNS TRIGGER AS $$
DECLARE
  run_id UUID;
  run_state TEXT;
BEGIN
  IF NEW.hackathon_id IS NULL THEN
    RETURN NEW; -- founding: no event, no rail to collide with
  END IF;

  SELECT id, state INTO run_id, run_state
  FROM keeperhub_payout_runs
  WHERE hackathon_id = NEW.hackathon_id
    AND pool = NEW.pool
  LIMIT 1;

  IF run_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % is being paid on the KeeperHub rail; it cannot also be settled on the Aptos rail',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'KH001',
            CONSTRAINT = 'settlements_one_rail_per_event_pool',
            DETAIL = format('keeperhub_payout_runs %s is %s', run_id, run_state),
            HINT = 'An event is paid on one rail only. Recording this settlement would pay the same people a second time.';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- BEFORE INSERT only, like 090300: a settlement's hackathon_id and pool are
-- never moved after the row exists.
DROP TRIGGER IF EXISTS trg_settlements_refuse_event_paid_on_keeperhub ON settlements;
CREATE TRIGGER trg_settlements_refuse_event_paid_on_keeperhub
  BEFORE INSERT ON settlements
  FOR EACH ROW
  EXECUTE FUNCTION settlements_refuse_event_paid_on_keeperhub();
