-- An event's pool is paid on ONE rail. A GrainHack results statement (Solana,
-- via the grainhack-signer) joins the two rails that already exclude each other:
-- KeeperHub (Base, keeperhub_payout_runs) and Aptos (settlements). Migrations
-- 20260916090300/090700/090800 keep those two apart with KH001/KH002; they are
-- untouched. This adds the third rail against both, in both directions:
--
--   GH001  a statement is refused when the event pool has a KeeperHub run or a
--          settlement;
--   GH002  a KeeperHub run is refused when the event pool has a statement;
--   GH003  a settlement is refused when the event pool has a statement.
--
-- Codes in class GH, beside KH: an implementation-defined class Postgres does
-- not use, recognised in Go by code rather than message text
-- (internal/grainhack, internal/keeperhubrail, internal/settlement).
--
-- # The predicate is mere existence
--
-- Same as KH001/KH002: a planned KeeperHub run can still dispatch, a failed one
-- may have paid, and a settlement can still be published. A statement is the
-- same: once issued, the signer may pay from it.
--
-- # Serialised per event pool
--
-- Each check runs after pg_advisory_xact_lock on the event pool, taken by all
-- three triggers with the same key. Without it, two transactions inserting on
-- different rails could each see the other's table empty and both commit. With
-- it, the second waits for the first to finish and then, under READ COMMITTED,
-- sees the first one's committed row.
--
-- Founding settlements (hackathon_id NULL) are not an event pool and return
-- immediately, before any lock.

CREATE OR REPLACE FUNCTION grainhack_rail_lock(hid UUID, pool_kind TEXT)
RETURNS VOID AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('grainlify-event-pool-rail:' || hid::text || ':' || pool_kind, 0));
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION grainhack_statement_refuse_other_rail()
RETURNS TRIGGER AS $$
DECLARE
  other_id UUID;
BEGIN
  PERFORM grainhack_rail_lock(NEW.hackathon_id, NEW.pool);

  SELECT id INTO other_id FROM keeperhub_payout_runs
  WHERE hackathon_id = NEW.hackathon_id AND pool = NEW.pool LIMIT 1;
  IF other_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % is being paid on the KeeperHub rail; a GrainHack results statement cannot be issued for it',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'GH001',
            CONSTRAINT = 'grainhack_results_statements_one_rail_per_event_pool',
            DETAIL = format('keeperhub_payout_runs %s', other_id),
            HINT = 'An event is paid on one rail only.';
  END IF;

  SELECT id INTO other_id FROM settlements
  WHERE hackathon_id = NEW.hackathon_id AND pool = NEW.pool LIMIT 1;
  IF other_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % is settled on the Aptos rail; a GrainHack results statement cannot be issued for it',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'GH001',
            CONSTRAINT = 'grainhack_results_statements_one_rail_per_event_pool',
            DETAIL = format('settlements %s', other_id),
            HINT = 'An event is paid on one rail only.';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_grainhack_statement_refuse_other_rail ON grainhack_results_statements;
CREATE TRIGGER trg_grainhack_statement_refuse_other_rail
  BEFORE INSERT ON grainhack_results_statements
  FOR EACH ROW EXECUTE FUNCTION grainhack_statement_refuse_other_rail();

CREATE OR REPLACE FUNCTION keeperhub_refuse_run_when_grainhack_statement()
RETURNS TRIGGER AS $$
DECLARE
  statement_id UUID;
BEGIN
  PERFORM grainhack_rail_lock(NEW.hackathon_id, NEW.pool);

  SELECT id INTO statement_id FROM grainhack_results_statements
  WHERE hackathon_id = NEW.hackathon_id AND pool = NEW.pool LIMIT 1;
  IF statement_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % has a GrainHack results statement; a KeeperHub run cannot be opened for it',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'GH002',
            CONSTRAINT = 'keeperhub_payout_runs_one_rail_per_event_pool',
            DETAIL = format('grainhack_results_statements %s', statement_id),
            HINT = 'An event is paid on one rail only. Opening this run would pay the same people a second time.';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_keeperhub_refuse_run_when_grainhack_statement ON keeperhub_payout_runs;
CREATE TRIGGER trg_keeperhub_refuse_run_when_grainhack_statement
  BEFORE INSERT ON keeperhub_payout_runs
  FOR EACH ROW EXECUTE FUNCTION keeperhub_refuse_run_when_grainhack_statement();

CREATE OR REPLACE FUNCTION settlements_refuse_event_with_grainhack_statement()
RETURNS TRIGGER AS $$
DECLARE
  statement_id UUID;
BEGIN
  IF NEW.hackathon_id IS NULL THEN
    RETURN NEW; -- founding: no event, no rail to collide with
  END IF;
  PERFORM grainhack_rail_lock(NEW.hackathon_id, NEW.pool);

  SELECT id INTO statement_id FROM grainhack_results_statements
  WHERE hackathon_id = NEW.hackathon_id AND pool = NEW.pool LIMIT 1;
  IF statement_id IS NOT NULL THEN
    RAISE EXCEPTION
      'hackathon % pool % has a GrainHack results statement; it cannot also be settled on the Aptos rail',
      NEW.hackathon_id, NEW.pool
      USING ERRCODE = 'GH003',
            CONSTRAINT = 'settlements_one_rail_per_event_pool',
            DETAIL = format('grainhack_results_statements %s', statement_id),
            HINT = 'An event is paid on one rail only. Recording this settlement would pay the same people a second time.';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_settlements_refuse_event_with_grainhack_statement ON settlements;
CREATE TRIGGER trg_settlements_refuse_event_with_grainhack_statement
  BEFORE INSERT ON settlements
  FOR EACH ROW EXECUTE FUNCTION settlements_refuse_event_with_grainhack_statement();
