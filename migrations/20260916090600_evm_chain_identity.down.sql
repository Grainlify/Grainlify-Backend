-- Removes base-sepolia only if nothing references it, for the same reason the
-- 'base' row's down migration is guarded: a payout destination is not something
-- a rollback may discard quietly.
ALTER TABLE keeperhub_payout_runs DROP COLUMN IF EXISTS evm_chain_id;
ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_evm_chain_id_unique;
ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_evm_has_chain_id;
DELETE FROM chain_configs
WHERE chain_id = 'base-sepolia'
  AND NOT EXISTS (SELECT 1 FROM contributor_addresses WHERE chain_id = 'base-sepolia')
  AND NOT EXISTS (SELECT 1 FROM hackathon_chain_pools WHERE chain_id = 'base-sepolia')
  AND NOT EXISTS (SELECT 1 FROM keeperhub_payout_runs WHERE chain_id = 'base-sepolia');
ALTER TABLE chain_configs DROP COLUMN IF EXISTS evm_chain_id;
