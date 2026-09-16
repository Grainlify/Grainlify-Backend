-- An event and pool is paid on Base or on Aptos, never both.
--
-- # What this half can and cannot guarantee
--
-- This is the KEEPERHUB SIDE of the exclusion, and it is deliberately only half
-- of it. It refuses to open a KeeperHub run for an event and pool that the
-- Aptos rail has already settled. The mirror - refusing to write a settlements
-- row for an event that has a KeeperHub run - is a change to the Aptos rail and
-- is NOT made here; it needs its own decision, because it puts a new refusal in
-- front of a path that already works.
--
-- Stated plainly so nobody reads more safety into this than it provides: until
-- that mirror exists, the two rails are mutually exclusive in one direction
-- only. Aptos-then-KeeperHub is blocked. KeeperHub-then-Aptos is not.
--
-- # Why a trigger, when this codebase has none
--
-- Not a preference. The rule spans two tables, so it cannot be a CHECK, and
-- there is no Go dispatch path to put it in yet - the client that would fire a
-- run is not built. A rule that exists only in code nobody has written yet is
-- not a rule, and this one guards the case where the same money goes out twice
-- by two different mechanisms.
--
-- When the dispatch path does land it should make the same check and say so in
-- a sentence a person can act on. This stays regardless: the handler is where
-- somebody is told, and the database is where it is true.

CREATE OR REPLACE FUNCTION keeperhub_refuse_run_when_already_settled()
RETURNS TRIGGER AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM settlements
    WHERE hackathon_id = NEW.hackathon_id
      AND pool = NEW.pool
  ) THEN
    RAISE EXCEPTION
      'hackathon % pool % already has a settlement: it is being paid on the Aptos rail, '
      'and opening a KeeperHub run would pay the same people a second time',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- BEFORE INSERT only.
--
-- Not on UPDATE: a run's hackathon_id and pool are never moved, and firing on
-- every status change would re-run the query for each leg progress write on a
-- run that was already legitimately opened.
DROP TRIGGER IF EXISTS trg_keeperhub_refuse_run_when_already_settled ON keeperhub_payout_runs;
CREATE TRIGGER trg_keeperhub_refuse_run_when_already_settled
  BEFORE INSERT ON keeperhub_payout_runs
  FOR EACH ROW
  EXECUTE FUNCTION keeperhub_refuse_run_when_already_settled();
