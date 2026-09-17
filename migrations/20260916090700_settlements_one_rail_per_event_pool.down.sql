-- Reopens the KeeperHub-then-Aptos path. Rolling this back removes a refusal,
-- not data, but it means an event already being paid on KeeperHub can once
-- again be settled on Aptos as well.
DROP TRIGGER IF EXISTS trg_settlements_refuse_event_paid_on_keeperhub ON settlements;
DROP FUNCTION IF EXISTS settlements_refuse_event_paid_on_keeperhub();
