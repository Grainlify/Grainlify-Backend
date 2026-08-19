-- 1. Exclusion is a first-class outcome, not a filter.
--
-- ineligible_reason answers "why did this person earn nothing". A member
-- excluded for having no payout address earned SOMETHING and has nowhere to
-- receive it - a different fact, and putting it in a column whose name says
-- there was no entitlement would mislead the first person to read it.
--
-- Kept separate for that reason rather than reusing the existing column with a
-- new value.
ALTER TABLE founding_settlement_lines
  ADD COLUMN IF NOT EXISTS excluded_reason TEXT;

ALTER TABLE founding_settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_excluded_reason_check;
ALTER TABLE founding_settlement_lines
  ADD CONSTRAINT founding_settlement_lines_excluded_reason_check
  CHECK (excluded_reason IS NULL OR excluded_reason IN ('no_address', 'no_github_account'));

-- A person cannot both have earned nothing and have earned something we cannot
-- deliver. If both columns are set, one of them is wrong.
ALTER TABLE founding_settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_one_reason;
ALTER TABLE founding_settlement_lines
  ADD CONSTRAINT founding_settlement_lines_one_reason
  CHECK (NOT (ineligible_reason IS NOT NULL AND excluded_reason IS NOT NULL));

-- 2. Seed chain_configs.
--
-- The table has existed and held zero rows, with no seed in any migration: a
-- config read by code and written by nobody. That is the same family as a column
-- that exists and is never written (merged_by) and a column that is populated and
-- never read (oauth_states.expires_at) - three instances now, all of them a
-- schema and a codebase that disagree about who is responsible for a value.
--
-- rpc_endpoint_ref is the NAME of the environment variable holding the endpoint,
-- not the endpoint. Nothing secret belongs in a migration, and an RPC URL with an
-- API key in it is a credential.
INSERT INTO chain_configs (chain_id, enabled, rpc_endpoint_ref, asset, min_confirmations, contract_address, explorer_url_template)
VALUES (
  'aptos-testnet',
  true,
  'APTOS_TESTNET_RPC_URL',
  jsonb_build_object(
    'symbol', 'USDC',
    'decimals', 6,
    'metadata_address', '0x69091fbab5f7d635ee7ac5098cf0c1efbe31d68fec0f2cd565e8d168daf52832'
  ),
  1,
  '0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9',
  'https://explorer.aptoslabs.com/txn/%s?network=testnet'
)
ON CONFLICT (chain_id) DO NOTHING;
