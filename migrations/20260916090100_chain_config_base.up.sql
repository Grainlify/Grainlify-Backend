-- Base joins chain_configs.
--
-- The table has held exactly one row, 'aptos-testnet'. A second chain is a row,
-- not a deploy - that is what this table is for - and the Base payout rail
-- cannot resolve an address family, or refuse a disabled chain, without one.
--
-- # enabled = true, with no contract address, on purpose
--
-- These two look contradictory and are not. They answer different questions:
--
--   enabled          -> may somebody REGISTER a Base payout address
--   contract_address -> where does a claim CLIENT send its transaction
--
-- Registration has to come first. Contributors register a destination long
-- before an event settles, and a chain that is switched off refuses
-- registration outright (payout.ChainFamilyFor), so leaving Base disabled until
-- a payout path exists would mean nobody can tell us where to send their money
-- until the moment we want to send it.
--
-- contract_address stays NULL because there is no Grainlify escrow contract on
-- Base and there may never be one: the KeeperHub rail transfers directly rather
-- than publishing a root somebody claims against. NULL is the honest value, and
-- it fails CLOSED - payout.ChainConfigFor refuses any row missing it, naming the
-- field, so the Aptos-shaped claim path cannot serve a half-answer for Base. If
-- an escrow ever exists here, that is the migration that adds it.
--
-- rpc_endpoint_ref is the NAME of an environment variable, never an endpoint. An
-- RPC URL with an API key in it is a credential, and nothing secret belongs in a
-- migration. Same rule as the Aptos row.
INSERT INTO chain_configs (
  chain_id, family, enabled, rpc_endpoint_ref, asset, min_confirmations,
  contract_address, explorer_url_template, network
)
VALUES (
  'base',
  'evm',
  true,
  'BASE_RPC_URL',
  -- USDC on Base mainnet, 6 decimals. The address is Circle's native USDC, not
  -- bridged USDbC, which is a different token with the same ticker - paying the
  -- wrong one is a payout nobody can spend as expected.
  jsonb_build_object(
    'symbol', 'USDC',
    'decimals', 6,
    'token_address', '0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913'
  ),
  -- Matches the Aptos row rather than inventing a number. Nothing on the Base
  -- rail reads this yet: KeeperHub reports a transaction's own confirmation, so
  -- when a leg-status reader lands it should either consume this column or this
  -- column should go - a value nobody reads is the failure mode this codebase
  -- keeps rediscovering.
  1,
  NULL,
  'https://basescan.org/tx/%s',
  'mainnet'
)
ON CONFLICT (chain_id) DO NOTHING;
