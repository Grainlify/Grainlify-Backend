-- Only removes the row if nothing depends on it.
--
-- Deleting a chain that addresses or pools reference would either fail on the
-- foreign key or orphan them, and a payout destination is not something a
-- rollback may discard quietly. The DELETE is therefore guarded rather than
-- unconditional: if anybody has registered a Base address, this leaves the row
-- alone and the rollback is a no-op that loses nothing.
DELETE FROM chain_configs
WHERE chain_id = 'base'
  AND NOT EXISTS (SELECT 1 FROM contributor_addresses WHERE chain_id = 'base')
  AND NOT EXISTS (SELECT 1 FROM hackathon_chain_pools WHERE chain_id = 'base');
