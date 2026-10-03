DROP TRIGGER IF EXISTS trg_settlements_refuse_event_with_grainhack_statement ON settlements;
DROP TRIGGER IF EXISTS trg_keeperhub_refuse_run_when_grainhack_statement ON keeperhub_payout_runs;
DROP TRIGGER IF EXISTS trg_grainhack_statement_refuse_other_rail ON grainhack_results_statements;
DROP FUNCTION IF EXISTS settlements_refuse_event_with_grainhack_statement();
DROP FUNCTION IF EXISTS keeperhub_refuse_run_when_grainhack_statement();
DROP FUNCTION IF EXISTS grainhack_statement_refuse_other_rail();
DROP FUNCTION IF EXISTS grainhack_rail_lock(UUID, TEXT);
