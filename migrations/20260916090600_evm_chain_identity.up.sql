-- A payout run knows the NUMERIC chain it pays on, and every settled
-- transaction is checked against it.
--
-- # The gap
--
-- chain_id is a label - 'base', 'aptos-testnet'. The KeeperHub workflow pays on
-- a network fixed inside the workflow itself (84532 for Base Sepolia). Nothing
-- tied the two together, and there was no Base Sepolia row at all: only 'base',
-- registered as mainnet. A Sepolia run released under 'base' would have been
-- recorded as a mainnet payment, and nothing would have noticed.
--
-- The check is against the chainId KeeperHub reports on each settled
-- transaction in the execution record - what actually happened - rather than a
-- configured value, which only says what we expected.
--
-- MERGE ORDER. Part of a sequence that must reach production in order and be
-- deployed once, after the last: #549, #550, #551, #552, #553, #554, #555, #556,
-- then this. golang-migrate applies only versions newer than the database's
-- current one, so deploying partway through silently skips any lower-numbered
-- migration merged afterwards - 20260916090100 would never run, the 'base' row
-- would never exist, and the UPDATE below would quietly match nothing.

-- 1. Every EVM chain row states its numeric chain id.
ALTER TABLE chain_configs ADD COLUMN IF NOT EXISTS evm_chain_id BIGINT;

UPDATE chain_configs SET evm_chain_id = 8453 WHERE chain_id = 'base' AND evm_chain_id IS NULL;

-- 2. Base Sepolia, which the payout workflow actually pays on.
--
-- Same shape as the 'base' row and for the same reasons: enabled so addresses
-- can be registered, no contract_address because this rail transfers directly
-- (ChainConfigFor refuses the row, naming the field), and rpc_endpoint_ref is
-- the NAME of an environment variable, never an endpoint.
INSERT INTO chain_configs (
  chain_id, family, enabled, rpc_endpoint_ref, asset, min_confirmations,
  contract_address, explorer_url_template, network, evm_chain_id
)
VALUES (
  'base-sepolia',
  'evm',
  true,
  'BASE_SEPOLIA_RPC_URL',
  -- Circle's test USDC on Base Sepolia, 6 decimals. The same token the payout
  -- workflow's transfer step names.
  jsonb_build_object(
    'symbol', 'USDC',
    'decimals', 6,
    'token_address', '0x036CbD53842c5426634e7929541eC2318f3dCF7e'
  ),
  1,
  NULL,
  'https://sepolia.basescan.org/tx/%s',
  'testnet',
  84532
)
ON CONFLICT (chain_id) DO NOTHING;

-- An EVM row without a numeric id cannot be verified against, so it is refused.
ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_evm_has_chain_id;
ALTER TABLE chain_configs
  ADD CONSTRAINT chain_configs_evm_has_chain_id
  CHECK (family <> 'evm' OR (evm_chain_id IS NOT NULL AND evm_chain_id > 0));

-- One row per numeric chain. Two labels for 84532 would make "which chain did
-- this run pay on" answerable two ways.
ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_evm_chain_id_unique;
ALTER TABLE chain_configs
  ADD CONSTRAINT chain_configs_evm_chain_id_unique UNIQUE (evm_chain_id);

-- 3. The run freezes the numeric id at plan time, like everything else it pays
-- against. NOT NULL is safe: nothing has ever written this table.
ALTER TABLE keeperhub_payout_runs
  ADD COLUMN IF NOT EXISTS evm_chain_id BIGINT NOT NULL CHECK (evm_chain_id > 0);
