DROP INDEX IF EXISTS idx_contributor_addresses_lookup;
DROP INDEX IF EXISTS idx_contributor_addresses_live;
DROP TABLE IF EXISTS contributor_addresses;

DROP INDEX IF EXISTS idx_auth_nonces_lookup;
CREATE INDEX IF NOT EXISTS idx_auth_nonces_lookup
  ON auth_nonces (wallet_type, address, expires_at);

ALTER TABLE auth_nonces DROP CONSTRAINT IF EXISTS auth_nonces_wallet_type_check;
ALTER TABLE auth_nonces
  ADD CONSTRAINT auth_nonces_wallet_type_check
  CHECK (wallet_type IN ('evm', 'stellar_ed25519', 'stellar_secp256k1'));

ALTER TABLE auth_nonces DROP CONSTRAINT IF EXISTS auth_nonces_purpose_check;
ALTER TABLE auth_nonces DROP COLUMN IF EXISTS purpose;
