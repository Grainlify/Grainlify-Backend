-- Per-event identity salts, and the leaf digests that let the salt go cold.
--
-- These are one migration because they are one design. A tree cannot be built
-- without a salt; a proof cannot be served after settlement without persisted
-- leaves; and until the leaves are persisted the salt can never be destroyed,
-- because rebuilding the tree to answer a proof request would need it.

-- 1. The salt.
--
-- One per settlement. Encrypted with SALT_ENC_KEY_B64 using AES-256-GCM, with
-- settlement_id as the associated data.
--
-- The AAD binding is not decoration. Without it a ciphertext copied from one
-- settlement's row into another's still decrypts, silently giving two events the
-- same salt - which destroys the one property a per-event salt exists to provide,
-- that leaves cannot be correlated across events.
CREATE TABLE IF NOT EXISTS payout_event_salts (
  settlement_id UUID PRIMARY KEY REFERENCES founding_settlements(id) ON DELETE RESTRICT,

  -- NULL once destroyed. The row is never deleted; see below.
  ciphertext BYTEA,
  nonce BYTEA,
  -- Which key encrypted this, so rotation can find rows it has not re-wrapped.
  key_version INT NOT NULL DEFAULT 1,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Destruction is a TOMBSTONE, never a DELETE.
  --
  -- A deleted row cannot answer "did this event have a salt, and when did we
  -- destroy it?" - and that is the audit trail for an act that is irreversible
  -- in both directions: it forecloses bulk deanonymisation by a future leaker,
  -- and forecloses our own ability to answer a contributor asking "was that
  -- leaf really mine?".
  --
  -- Destruction is deliberate and never scheduled, for the same reason the
  -- sweep deadline is an act rather than a clock: a timer fires while nobody is
  -- watching. The reason is recorded next to the tombstone because a
  -- destruction with no stated cause is indistinguishable from an accident.
  destroyed_at TIMESTAMPTZ,
  destroyed_reason TEXT,

  -- Live rows carry a salt; destroyed rows carry none and carry a reason.
  -- Neither half of a tombstone is optional.
  CONSTRAINT payout_event_salts_live_or_destroyed CHECK (
    (destroyed_at IS NULL     AND ciphertext IS NOT NULL AND nonce IS NOT NULL AND destroyed_reason IS NULL)
    OR
    (destroyed_at IS NOT NULL AND ciphertext IS NULL     AND nonce IS NULL     AND destroyed_reason IS NOT NULL)
  ),
  CONSTRAINT payout_event_salts_reason_not_blank CHECK (
    destroyed_reason IS NULL OR length(btrim(destroyed_reason)) > 0
  )
);

-- 2. The leaves.
--
-- Persisted so a proof can be served without the salt. Lookup is BY ADDRESS,
-- deliberately: storing user_id here would make proof serving trivial and salt
-- destruction theatre, because the login-to-leaf mapping the salt protects
-- would then sit in this table in plaintext.
--
-- Address lookup costs nothing, because the contributor must connect that
-- wallet to claim anyway and proves control of it with the same signature
-- machinery. It also means this table publishes nothing the chain does not
-- already publish: every address and amount in it appears on-chain at claim
-- time.
CREATE TABLE IF NOT EXISTS claim_leaves (
  settlement_id UUID NOT NULL REFERENCES founding_settlements(id) ON DELETE RESTRICT,

  -- Position in the sorted tree. Stored, not recomputed: a proof must be
  -- reproducible, and recomputing the order needs the full leaf set in the same
  -- sort - which is exactly what we may no longer be able to rebuild.
  leaf_index INT NOT NULL CHECK (leaf_index >= 0),

  leaf_hash BYTEA NOT NULL CHECK (octet_length(leaf_hash) = 32),
  identity_hash BYTEA NOT NULL CHECK (octet_length(identity_hash) = 32),

  -- The FROZEN copy of the payout address. contributor_addresses holds current
  -- intent and may change; this is what the leaf commits to and must not.
  claim_address TEXT NOT NULL CHECK (length(btrim(claim_address)) > 0),
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  pool TEXT NOT NULL CHECK (pool IN ('contributor', 'maintainer')),

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (settlement_id, leaf_index)
);

-- One leaf digest per settlement: a duplicate means the tree was built twice or
-- built wrong, and a duplicated leaf is a double payment on a promoted-node tree.
CREATE UNIQUE INDEX IF NOT EXISTS idx_claim_leaves_hash
  ON claim_leaves (settlement_id, leaf_hash);

-- The serving path: "what is owed to this address in this settlement?"
CREATE INDEX IF NOT EXISTS idx_claim_leaves_address
  ON claim_leaves (settlement_id, lower(claim_address));

-- One address appears at most once per settlement per pool. Two leaves paying
-- one address in one pool is an apportionment bug, and the tree would pay it
-- twice.
CREATE UNIQUE INDEX IF NOT EXISTS idx_claim_leaves_address_once
  ON claim_leaves (settlement_id, pool, lower(claim_address));
