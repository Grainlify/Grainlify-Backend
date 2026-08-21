-- Gas sponsorship for contributor claims. See docs/SCOPE-sponsored-claim.md and
-- Grainlify-Backend#537.

-- 1. The sender's public key, so a claim can be SIMULATED BEFORE IT IS SIGNED.
--
-- Verified empirically against testnet: an Aptos fee-payer transaction simulates
-- with a ZERO signature and returns the real result - a valid claim executes
-- (success, gas_used 4431), a wrong amount aborts. What the node does check is
-- that the public key matches the account's authentication key, so the key is
-- required and the signature is not.
--
-- That turns "this transaction would fail" from a refusal after signing into a
-- check before it: the person never sees a wallet prompt for an action we
-- already know aborts.
--
-- We already receive this at registration and verify it derives to the address.
-- We simply never stored it.
ALTER TABLE contributor_addresses ADD COLUMN IF NOT EXISTS public_key TEXT;

-- 2. Every sponsorship attempt, for rate limiting and for the record.
--
-- A row is written when we decide, not when the chain answers, because the cost
-- being bounded is OURS: a submitted transaction that aborts still charges the
-- fee payer, so the thing worth counting is submissions, not successes.
CREATE TABLE IF NOT EXISTS sponsored_claims (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  settlement_id UUID NOT NULL REFERENCES settlements(id) ON DELETE RESTRICT,
  leaf_hash BYTEA NOT NULL CHECK (octet_length(leaf_hash) = 32),

  -- One name per outcome. 'refused_*' rows cost no gas and are kept because a
  -- burst of them is the shape of an attack, which is exactly what a rate limit
  -- is meant to notice.
  outcome TEXT NOT NULL CHECK (outcome IN (
    'submitted',
    'refused_already_claimed',
    'refused_simulation_failed',
    'refused_rate_limited',
    'refused_low_balance'
  )),
  tx_hash TEXT,
  gas_octas BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The rate-limit query: how many submissions has this person made recently.
CREATE INDEX IF NOT EXISTS idx_sponsored_claims_user_recent
  ON sponsored_claims (user_id, created_at DESC);

-- A settlement pays a person at most one leaf, so one SUBMITTED sponsorship per
-- person per settlement is all that can ever be legitimate. Enforced rather than
-- counted: a second submission for the same leaf is either a double-click or an
-- attempt to make us pay twice, and neither should reach the chain.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sponsored_claims_one_submission
  ON sponsored_claims (settlement_id, leaf_hash)
  WHERE outcome = 'submitted';
