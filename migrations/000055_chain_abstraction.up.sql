-- GrainHack on-chain spec §6, stage 1. Chain-agnostic throughout: nothing
-- here names a specific chain, and adding Starknet/Flare/Solana later must
-- require a new row rather than a new column.

CREATE TABLE IF NOT EXISTS chain_configs (
  chain_id TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL DEFAULT false,
  rpc_endpoint_ref TEXT,
  asset JSONB NOT NULL,
  -- Per chain, because Soroban finality is not Solana's is not an EVM
  -- chain's. Nothing is final until the adapter reports this depth (§3.3).
  min_confirmations INT NOT NULL DEFAULT 1 CHECK (min_confirmations >= 1),
  contract_address TEXT,
  explorer_url_template TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The source of truth for which chains an event runs on (§6). Every on-chain
-- operation is driven from this table, so a single-chain event is just the
-- same loop with one row.
CREATE TABLE IF NOT EXISTS hackathon_chain_pools (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  chain_id TEXT NOT NULL REFERENCES chain_configs(chain_id),

  -- Held separately, never one balance with accounting (§5.1), mirroring the
  -- off-chain guarantee that maintainer payouts cannot draw on contributor
  -- funds.
  contributor_pool NUMERIC(30,0) NOT NULL DEFAULT 0,
  maintainer_pool NUMERIC(30,0) NOT NULL DEFAULT 0,
  -- Minor units, so the on-chain integer and this row cannot disagree about
  -- precision (§9).
  asset_decimals INT NOT NULL DEFAULT 6,

  escrow_ref TEXT,
  funding_tx TEXT,
  funded_at TIMESTAMPTZ,
  -- Exactly three on-chain states plus the pre-submission one (§3.4). A
  -- confirmed action that reorgs out returns to 'submitted'.
  state TEXT NOT NULL DEFAULT 'unfunded'
    CHECK (state IN ('unfunded', 'submitted', 'confirmed', 'failed')),
  last_error TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, chain_id)
);
CREATE INDEX IF NOT EXISTS idx_chain_pools_hackathon ON hackathon_chain_pools(hackathon_id);

CREATE TABLE IF NOT EXISTS chain_commitments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  chain_id TEXT NOT NULL REFERENCES chain_configs(chain_id),
  kind TEXT NOT NULL CHECK (kind IN ('config_hash', 'draw_commit', 'draw_reveal', 'claim_root')),
  -- Issue ref for draw commit/reveal, null otherwise.
  subject_ref TEXT,
  value BYTEA NOT NULL,
  tx_hash TEXT,
  submitted_at TIMESTAMPTZ,
  confirmed_at TIMESTAMPTZ,
  state TEXT NOT NULL DEFAULT 'submitted'
    CHECK (state IN ('submitted', 'confirmed', 'failed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One live commitment per (event, chain, kind, subject). A second draw commit
-- for the same issue would make "which seed was committed?" unanswerable,
-- which is the entire guarantee.
CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_commitments_unique
  ON chain_commitments(hackathon_id, chain_id, kind, COALESCE(subject_ref, ''));

-- Append-only log of every unsigned transaction built, submitted, confirmed
-- or failed, with actor and reason. Same role as hackathon_model_calls: this
-- is what makes an on-chain dispute answerable months later (§6).
CREATE TABLE IF NOT EXISTS chain_operations (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE SET NULL,
  chain_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  subject_ref TEXT,
  state TEXT NOT NULL CHECK (state IN ('built', 'submitted', 'confirmed', 'failed', 'reorged')),
  tx_hash TEXT,
  -- Human-readable summary of what the transaction would do. A signer
  -- approving an opaque blob is a signer approving anything.
  summary TEXT,
  actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  reason TEXT,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_chain_operations_hackathon
  ON chain_operations(hackathon_id, created_at DESC);
