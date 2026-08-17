DROP INDEX IF EXISTS idx_chain_operations_pending;
DROP INDEX IF EXISTS idx_chain_operations_event;
DROP INDEX IF EXISTS idx_chain_operations_attempt;

ALTER TABLE chain_operations
  DROP CONSTRAINT IF EXISTS chain_operations_leaf_hash_iff_claim;
ALTER TABLE chain_operations
  DROP CONSTRAINT IF EXISTS chain_operations_claims_are_observed_not_submitted;

ALTER TABLE chain_operations
  DROP COLUMN IF EXISTS leaf_hash,
  DROP COLUMN IF EXISTS event_ref;

-- Back to the original state set. Any row already in 'paid' would violate this,
-- so it is dropped rather than reverted - and since nothing has ever written to
-- this table, there is nothing to lose.
DELETE FROM chain_operations WHERE state = 'paid';
ALTER TABLE chain_operations DROP CONSTRAINT IF EXISTS chain_operations_state_check;
ALTER TABLE chain_operations
  ADD CONSTRAINT chain_operations_state_check
  CHECK (state IN ('built', 'submitted', 'confirmed', 'failed', 'reorged'));
