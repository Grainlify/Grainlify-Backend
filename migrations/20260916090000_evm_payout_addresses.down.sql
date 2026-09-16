-- Reverting is only safe while every stored address is still Aptos-shaped.
--
-- The address CHECK this restores rejects any 40-hex EVM row, so if one exists
-- the ALTER fails and the rollback stops - deliberately. The alternative is
-- dropping rows that say where somebody's money goes, which a down migration
-- must never do quietly.
DROP INDEX IF EXISTS idx_contributor_addresses_lookup;
CREATE INDEX IF NOT EXISTS idx_contributor_addresses_lookup
  ON contributor_addresses (chain_id, address) WHERE superseded_at IS NULL;

DROP INDEX IF EXISTS idx_contributor_addresses_one_account;
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributor_addresses_one_account
  ON contributor_addresses (chain_id, address)
  WHERE superseded_at IS NULL;

ALTER TABLE contributor_addresses DROP CONSTRAINT IF EXISTS contributor_addresses_address_matches_family;
ALTER TABLE contributor_addresses
  ADD CONSTRAINT contributor_addresses_address_check CHECK (address ~ '^0x[0-9a-f]{64}$');

ALTER TABLE contributor_addresses DROP CONSTRAINT IF EXISTS contributor_addresses_chain_family_known;
ALTER TABLE contributor_addresses DROP COLUMN IF EXISTS chain_family;

ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_family_known;
ALTER TABLE chain_configs DROP COLUMN IF EXISTS family;
