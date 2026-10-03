CREATE OR REPLACE FUNCTION grainhack_results_refuse_change()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'grainhack results statements are immutable once issued (% on %)', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'GH010',
          HINT = 'Issue a new statement that supersedes this one instead.';
END;
$$ LANGUAGE plpgsql;

DROP TABLE IF EXISTS grainhack_results_retention_log;
ALTER TABLE grainhack_results_statements DROP CONSTRAINT IF EXISTS grainhack_results_statements_signature_check;
ALTER TABLE grainhack_results_statements ADD CONSTRAINT grainhack_results_statements_signature_check
  CHECK (length(signature) > 0);
ALTER TABLE grainhack_results_statements DROP COLUMN IF EXISTS redacted_at;
DROP INDEX IF EXISTS grainhack_results_statement_lines_id;
ALTER TABLE grainhack_results_statement_lines DROP COLUMN IF EXISTS id;
