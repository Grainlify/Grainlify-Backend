-- GrainHack results statements under the payout-record retention period.
--
-- Statements are immutable (GH010, 20261003120000): a signed document that
-- disagreed with the table holding it would be worse than useless. Erasure
-- respects that - an erased account's statement lines are payout records and
-- are kept, login and all, because the login is inside the signed JSON.
--
-- But the Terms promise that an erased account's payout records are erased
-- five years after the payment (internal/erasure/retention.go). So there is
-- exactly one way past GH010, and it is narrow:
--
--   * only inside a transaction that has set grainlify.grainhack_results_retention
--     to its own transaction id with set_config(..., true) - a session-wide SET,
--     or a value left from another transaction, never matches;
--   * DELETE of a statement line only when the line's account is erased;
--   * UPDATE of a statement only to redact it (canonical_json, signature,
--     redacted_at; every other column unchanged) once no line of an erased
--     account is left in it - the document still names whoever the deleted
--     lines named, so it goes the same way, and keeps the other winners' lines;
--   * every use is written to grainhack_results_retention_log, which names a
--     statement and a transaction, never a person.
--
-- Everything else is refused exactly as before. Only the retention pass
-- (erasure.Retention.purgePayoutRecords) sets the setting.

-- A statement line had no single-column key; the retention pass selects and
-- acts on rows by id, the same as every other payout record.
ALTER TABLE grainhack_results_statement_lines
  ADD COLUMN IF NOT EXISTS id UUID NOT NULL DEFAULT gen_random_uuid();
CREATE UNIQUE INDEX IF NOT EXISTS grainhack_results_statement_lines_id
  ON grainhack_results_statement_lines (id);

-- Set when the retention pass redacted the document. The signature is then
-- emptied: it was over bytes that are no longer stored.
ALTER TABLE grainhack_results_statements ADD COLUMN IF NOT EXISTS redacted_at TIMESTAMPTZ;
ALTER TABLE grainhack_results_statements DROP CONSTRAINT IF EXISTS grainhack_results_statements_signature_check;
ALTER TABLE grainhack_results_statements ADD CONSTRAINT grainhack_results_statements_signature_check
  CHECK (length(signature) > 0 OR redacted_at IS NOT NULL);

CREATE TABLE IF NOT EXISTS grainhack_results_retention_log (
  id BIGSERIAL PRIMARY KEY,
  op TEXT NOT NULL CHECK (op IN ('line_deleted', 'statement_redacted')),
  statement_id UUID NOT NULL,
  txid BIGINT NOT NULL,
  at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- plpgsql does not promise to short-circuit AND, and reading a column the
-- other table does not have is an error, so each table has its own branch.
CREATE OR REPLACE FUNCTION grainhack_results_refuse_change()
RETURNS TRIGGER AS $$
BEGIN
  IF current_setting('grainlify.grainhack_results_retention', true) = txid_current()::text THEN
    IF TG_TABLE_NAME = 'grainhack_results_statement_lines' THEN
      IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM users u WHERE u.id = OLD.user_id AND u.erased_at IS NOT NULL) THEN
          INSERT INTO grainhack_results_retention_log (op, statement_id, txid)
            VALUES ('line_deleted', OLD.statement_id, txid_current());
          RETURN OLD;
        END IF;
      END IF;
    ELSIF TG_TABLE_NAME = 'grainhack_results_statements' THEN
      IF TG_OP = 'UPDATE' THEN
        IF NEW.redacted_at IS NOT NULL
           AND (to_jsonb(NEW) - ARRAY['canonical_json', 'signature', 'redacted_at'])
             = (to_jsonb(OLD) - ARRAY['canonical_json', 'signature', 'redacted_at'])
           AND NOT EXISTS (SELECT 1 FROM grainhack_results_statement_lines l JOIN users u ON u.id = l.user_id
                           WHERE l.statement_id = OLD.id AND u.erased_at IS NOT NULL) THEN
          INSERT INTO grainhack_results_retention_log (op, statement_id, txid)
            VALUES ('statement_redacted', OLD.id, txid_current());
          RETURN NEW;
        END IF;
      END IF;
    END IF;
  END IF;
  RAISE EXCEPTION 'grainhack results statements are immutable once issued (% on %)', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'GH010',
          HINT = 'Issue a new statement that supersedes this one instead.';
END;
$$ LANGUAGE plpgsql;
