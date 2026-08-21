-- The network label a client needs to reach this chain.
--
-- A LABEL, deliberately, not a URL.
--
-- rpc_endpoint_ref holds the NAME of an environment variable precisely so that
-- no endpoint with an API key in it ends up in a migration, so it cannot be
-- served verbatim. And a URL we serve is a URL we are on the hook for keeping
-- alive: clients would cache it, and rotating a provider would break claims for
-- everyone still holding the old one.
--
-- "testnet" / "mainnet" is not a secret, does not rot when a provider changes,
-- and the Aptos SDK already knows the endpoints for both.
--
-- If public-node rate limits ever make claims fail intermittently, the answer is
-- a proxy we own rather than a served URL. That future does not get to decide
-- this column.
ALTER TABLE chain_configs ADD COLUMN IF NOT EXISTS network TEXT;

UPDATE chain_configs SET network = 'testnet' WHERE chain_id = 'aptos-testnet' AND network IS NULL;

-- Nullable, like contract_address, and treated the same way: a NULL is a
-- half-finished seed and must be an error at read time, never an empty string a
-- client will use. See internal/payout.ChainConfigFor.
