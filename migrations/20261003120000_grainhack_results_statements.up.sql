-- GrainHack results statements: the signed record of who is owed what from an
-- event's pool, issued by an admin once the event is releasable and handed to
-- the bounty agent and the grainhack-signer, which pay from it on Solana.
--
-- Contract: drafts/grainhack-payout-contract.md section 1. The canonical JSON
-- and its Ed25519 signature are stored exactly as issued; the columns beside
-- them are the same facts in queryable form and are written from the same
-- values in the same transaction (internal/grainhack).
--
-- # Immutable once issued
--
-- A statement is evidence of what the backend promised the signer. Editing one
-- after the fact would leave a signed document that disagrees with the table
-- that claims to hold it, so UPDATE and DELETE are refused on both tables.
-- A change of circumstances (a held winner's KYC clearing) is a NEW statement
-- that names the one it supersedes; the chain is the history.
--
-- TRUNCATE is deliberately not blocked: the test suites reset tables with
-- TRUNCATE ... CASCADE, and an operator who can TRUNCATE can also drop a
-- trigger.
--
-- # The chain
--
-- One root statement per event and pool (supersedes IS NULL), and each
-- statement superseded at most once (supersedes is UNIQUE), so the chain is a
-- line and "the latest" has exactly one answer. A superseding statement must
-- be for the same event, pool, computation and network as the one it replaces:
-- enforced by the composite foreign key, not by convention.
--
-- # The lines add up
--
-- A deferred constraint trigger checks, at commit, that a statement's lines
-- sum to its pool_minor. That is the contract's invariant, and it is also what
-- stops a line being added to a statement after it was issued: any later line
-- (amount > 0 by CHECK) breaks the sum.

CREATE TABLE IF NOT EXISTS grainhack_results_statements (
  id UUID PRIMARY KEY,
  supersedes UUID UNIQUE,
  hackathon_id UUID NOT NULL REFERENCES hackathons(id),
  pool TEXT NOT NULL CHECK (pool IN ('contributor', 'maintainer')),
  -- hackathon_payout_runs.id: the computation the amounts were taken from.
  computation_id UUID NOT NULL REFERENCES hackathon_payout_runs(id),
  currency TEXT NOT NULL CHECK (currency = 'USDC'),
  network TEXT NOT NULL CHECK (network IN ('solana-devnet', 'solana-mainnet')),
  pool_minor NUMERIC(78, 0) NOT NULL CHECK (pool_minor > 0),
  -- Exactly the bytes that were signed, after the domain prefix.
  canonical_json TEXT NOT NULL CHECK (length(canonical_json) > 0),
  -- base64 (standard) of the 64-byte Ed25519 signature over
  -- "grainlify-grainhack-results:v1\n" || canonical_json.
  signature TEXT NOT NULL CHECK (length(signature) > 0),
  -- base64 of the 32-byte public key that signed it, so a key rotation does
  -- not leave old statements unverifiable by anyone reading this table.
  signing_public_key TEXT NOT NULL CHECK (length(signing_public_key) > 0),
  issued_by UUID NOT NULL REFERENCES users(id),
  issued_at TIMESTAMPTZ NOT NULL,
  CHECK (supersedes IS NULL OR supersedes <> id),
  UNIQUE (id, hackathon_id, pool, computation_id, network),
  FOREIGN KEY (supersedes, hackathon_id, pool, computation_id, network)
    REFERENCES grainhack_results_statements (id, hackathon_id, pool, computation_id, network)
);

CREATE UNIQUE INDEX IF NOT EXISTS grainhack_results_statements_one_root
  ON grainhack_results_statements (hackathon_id, pool) WHERE supersedes IS NULL;

CREATE INDEX IF NOT EXISTS idx_grainhack_results_statements_event
  ON grainhack_results_statements (hackathon_id, pool, issued_at DESC);

CREATE TABLE IF NOT EXISTS grainhack_results_statement_lines (
  statement_id UUID NOT NULL REFERENCES grainhack_results_statements(id),
  github_user_id BIGINT NOT NULL CHECK (github_user_id > 0),
  user_id UUID NOT NULL REFERENCES users(id),
  login TEXT NOT NULL CHECK (length(login) > 0),
  amount_minor NUMERIC(78, 0) NOT NULL CHECK (amount_minor > 0),
  status TEXT NOT NULL CHECK (status IN ('payable', 'held_kyc')),
  PRIMARY KEY (statement_id, github_user_id),
  UNIQUE (statement_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_grainhack_results_statement_lines_user
  ON grainhack_results_statement_lines (user_id);

CREATE OR REPLACE FUNCTION grainhack_results_refuse_change()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'grainhack results statements are immutable once issued (% on %)', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'GH010',
          HINT = 'Issue a new statement that supersedes this one instead.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_grainhack_results_statements_immutable ON grainhack_results_statements;
CREATE TRIGGER trg_grainhack_results_statements_immutable
  BEFORE UPDATE OR DELETE ON grainhack_results_statements
  FOR EACH ROW EXECUTE FUNCTION grainhack_results_refuse_change();

DROP TRIGGER IF EXISTS trg_grainhack_results_statement_lines_immutable ON grainhack_results_statement_lines;
CREATE TRIGGER trg_grainhack_results_statement_lines_immutable
  BEFORE UPDATE OR DELETE ON grainhack_results_statement_lines
  FOR EACH ROW EXECUTE FUNCTION grainhack_results_refuse_change();

CREATE OR REPLACE FUNCTION grainhack_results_check_sum()
RETURNS TRIGGER AS $$
DECLARE
  sid UUID;
  want NUMERIC;
  got NUMERIC;
BEGIN
  IF TG_TABLE_NAME = 'grainhack_results_statements' THEN
    sid := NEW.id;
  ELSE
    sid := NEW.statement_id;
  END IF;

  SELECT pool_minor INTO want FROM grainhack_results_statements WHERE id = sid;
  SELECT COALESCE(sum(amount_minor), 0) INTO got
    FROM grainhack_results_statement_lines WHERE statement_id = sid;

  IF want IS NULL OR got <> want THEN
    RAISE EXCEPTION 'grainhack results statement % lines sum to % but its pool is %', sid, got, want
      USING ERRCODE = 'GH011',
            HINT = 'Every line, payable or held, is part of the pool; the sum must equal pool_minor exactly.';
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_grainhack_results_statements_sum ON grainhack_results_statements;
CREATE CONSTRAINT TRIGGER trg_grainhack_results_statements_sum
  AFTER INSERT ON grainhack_results_statements
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION grainhack_results_check_sum();

DROP TRIGGER IF EXISTS trg_grainhack_results_statement_lines_sum ON grainhack_results_statement_lines;
CREATE CONSTRAINT TRIGGER trg_grainhack_results_statement_lines_sum
  AFTER INSERT ON grainhack_results_statement_lines
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION grainhack_results_check_sum();
