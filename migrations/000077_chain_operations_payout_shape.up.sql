-- Reshape chain_operations for a payout that is not a hackathon, and make one
-- dangerous row impossible to store.
--
-- The table was created by 000055 and has never been read or written by any Go
-- file, so nothing here has to preserve existing data - there is none.

-- 1. `paid` is a distinct state from `confirmed`.
--
-- `confirmed` means the transaction is on chain. `paid` means the reconciler
-- read chain state and the leaf is settled. Collapsing those two is how a
-- double-pay happens: a row that says "the transaction landed" is not a row
-- that says "this person has their money", and only the second one is safe to
-- stop retrying on.
ALTER TABLE chain_operations DROP CONSTRAINT IF EXISTS chain_operations_state_check;
ALTER TABLE chain_operations
  ADD CONSTRAINT chain_operations_state_check
  CHECK (state IN ('built', 'submitted', 'confirmed', 'paid', 'failed', 'reorged'));

-- 2. An event reference that does not require a hackathon.
--
-- The founding pool is not a hackathon, so hackathon_id is not an event key.
-- event_ref is the settlement id: a founding_settlements row is created once,
-- after a human has read the dry run, and is exactly the thing a root commits
-- to. It is also what the on-chain escrow carries as its event_id, so one value
-- identifies the event in the database, in the contract, and here.
--
-- No foreign key. This table must be able to record an attempt against an event
-- whose settlement row was later removed, because what the chain did does not
-- stop being true when our records change.
ALTER TABLE chain_operations
  ADD COLUMN IF NOT EXISTS event_ref UUID,
  ADD COLUMN IF NOT EXISTS leaf_hash BYTEA;

-- 3. What makes a retry safe.
--
-- Without this, a process that crashes between writing the row and submitting
-- can write a second row on restart, and the reconciler then sees two attempts
-- at one operation with no way to tell whether that means one or two
-- transactions.
CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_operations_attempt
  ON chain_operations (chain_id, event_ref, kind, COALESCE(leaf_hash, ''::bytea))
  WHERE event_ref IS NOT NULL;

-- 4. THE ONE THAT MATTERS: a claim row can never be in a retryable state.
--
-- This table holds two lifecycles that look identical and are not.
--
--   fund, publish_root, sweep_*  -- WE submit these. The row is written before
--                                   submission, as a record of intent, and a
--                                   crashed attempt is resumed by retrying it.
--
--   claim                        -- THE CONTRIBUTOR submits this. The row is a
--                                   record of OBSERVATION, written when the
--                                   reconciler first sees the claim on chain.
--
-- A reconciler that picked up a stale `built` claim row and "recovered" it would
-- be attempting to move funds to a contributor who did not sign for it: a push
-- payout, which is the one architecture this system forbids. It would arrive in
-- review looking like careful crash-recovery logic, because that is exactly what
-- it would be - applied to the wrong lifecycle.
--
-- Relying on the reconciler branching correctly on `kind` is relying on every
-- future author noticing the distinction. This constraint means the retry query,
-- which selects rows in 'built' or 'submitted', **cannot return a claim row**,
-- because such a row cannot exist. The mistake stops being a discipline and
-- becomes a storage error.
--
-- DO NOT DROP THIS BECAUSE internal/chainops ALREADY GUARDS IT.
--
-- It does, and that is not a reason. The two layers fail in different
-- directions, and neither covers the other's case:
--
--   * The Go gate (a Submittable that cannot be constructed from a claim) is
--     bypassed by anybody writing their own SQL, or a new query, or a script.
--     Plenty of code will touch this table without importing that package.
--
--   * This constraint is bypassed by a database restored, branched or migrated
--     without it - at which point the only thing standing between a stale row
--     and a push payout is a Go type somebody has to remember to route through.
--
-- The pair is deliberate. Reading one and concluding the other is redundant is
-- the specific mistake this paragraph exists to interrupt: they look like
-- duplicates and are not.
ALTER TABLE chain_operations
  ADD CONSTRAINT chain_operations_claims_are_observed_not_submitted
  CHECK (kind <> 'claim' OR state IN ('confirmed', 'paid', 'reorged'));

-- 5. The two shapes are structurally distinct.
--
-- A claim is per leaf; everything else is per event. Requiring exactly one of
-- those to be true stops a claim row being written without the leaf it settles,
-- and stops a publish_root row carrying a leaf it has nothing to do with.
ALTER TABLE chain_operations
  ADD CONSTRAINT chain_operations_leaf_hash_iff_claim
  CHECK ((kind = 'claim') = (leaf_hash IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_chain_operations_event
  ON chain_operations (event_ref, kind) WHERE event_ref IS NOT NULL;

-- Pending work the reconciler polls: only ever non-claim rows, by constraint 4.
CREATE INDEX IF NOT EXISTS idx_chain_operations_pending
  ON chain_operations (state, created_at) WHERE state IN ('built', 'submitted');
