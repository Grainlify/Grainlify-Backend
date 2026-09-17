-- Restores the function exactly as 20260916090300 created it. Note that the Go
-- side recognises only KH002, so rolling this back without rolling back the
-- code makes the refusal surface as a generic insert failure.
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
