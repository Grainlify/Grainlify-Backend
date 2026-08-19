-- Payout destinations, kept separate from sign-in wallets.
--
-- `wallets` answers "prove you hold this key" - authentication. A payout address
-- answers "send money here" - a destination. Merged, a wallet somebody added to
-- sign in silently becomes somewhere money gets sent.
--
-- The sharper form of the same problem: a payout address must be FROZEN at
-- leaf-build time, because the leaf commits to it, while `wallets` is mutable
-- current state. One row means changing your sign-in wallet either retroactively
-- rewrites where past money was headed, or does not and the two quietly
-- disagree. contributor_addresses holds current intent; claim_leaves holds the
-- frozen copy.

-- 1. Domain separation for the challenge.
--
-- auth_nonces already solves issuance, expiry, single use and uniqueness, so it
-- is reused rather than duplicated. But a nonce issued to sign in must not be
-- replayable to register a payout destination: the two now mean different
-- things, and the challenge has to say which one it is.
ALTER TABLE auth_nonces
  ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'signin';

ALTER TABLE auth_nonces DROP CONSTRAINT IF EXISTS auth_nonces_purpose_check;
ALTER TABLE auth_nonces
  ADD CONSTRAINT auth_nonces_purpose_check
  CHECK (purpose IN ('signin', 'payout_address'));

-- Aptos joins here and NOT in wallets.wallet_type: nothing signs in with an
-- Aptos wallet, so the authentication table needs no new value. Nonces are
-- machinery; addresses are storage.
ALTER TABLE auth_nonces DROP CONSTRAINT IF EXISTS auth_nonces_wallet_type_check;
ALTER TABLE auth_nonces
  ADD CONSTRAINT auth_nonces_wallet_type_check
  CHECK (wallet_type IN ('evm', 'stellar_ed25519', 'stellar_secp256k1', 'aptos_ed25519'));

-- A nonce is single-use WITHIN a purpose, and the uniqueness must include it.
DROP INDEX IF EXISTS idx_auth_nonces_lookup;
CREATE INDEX IF NOT EXISTS idx_auth_nonces_lookup
  ON auth_nonces (wallet_type, address, purpose, expires_at);

-- 2. The destinations themselves.
CREATE TABLE IF NOT EXISTS contributor_addresses (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

  -- Which chain this address is for, e.g. 'aptos-testnet'.
  chain_id TEXT NOT NULL CHECK (length(btrim(chain_id)) > 0),

  -- Canonical form only. An Aptos address may be written short (0x1) or padded
  -- to 64 hex characters; two spellings of one address would defeat the unique
  -- index below and produce a leaf committing to a string the wallet will not
  -- match. Stored 0x + 64 lowercase hex, always.
  address TEXT NOT NULL CHECK (address ~ '^0x[0-9a-f]{64}$'),

  -- Proof of control. An address is only ever written after a verified
  -- signature, so this is NOT NULL: an unverified payout destination is the
  -- thing this table exists to prevent.
  verified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  verified_nonce TEXT NOT NULL,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- History is retained rather than overwritten: "where was this person's money
  -- going in March" is a question a dispute asks, and an UPDATE in place cannot
  -- answer it.
  superseded_at TIMESTAMPTZ
);

-- One LIVE payout address per chain per person. Per-chain rather than
-- per-settlement is deliberate: the freeze into claim_leaves already gives
-- per-settlement semantics, so per-settlement storage would only add the
-- ability to change an address for a tree not yet built - identical to changing
-- it now - while making a contributor repeat wallet setup per event, which is
-- the version nobody completes.
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributor_addresses_live
  ON contributor_addresses (user_id, chain_id)
  WHERE superseded_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_contributor_addresses_lookup
  ON contributor_addresses (chain_id, address) WHERE superseded_at IS NULL;
