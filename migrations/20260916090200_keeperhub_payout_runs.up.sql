-- The ledger for a KeeperHub payout run. THE LEG IS THE UNIT OF TRUTH.
--
-- # Why this table exists at all
--
-- Proved on chain, not inferred: a KeeperHub For Each payout that fails partway
-- cannot be resumed. Re-running it restarts at iteration 0 and re-pays every leg
-- that already settled. Observed 2026-09-15 on Base Sepolia - execution
-- 8om5c2jiw0jyoxk2k8npq paid recipient 1 and failed on recipient 2; the retry,
-- 9hqtgyzgmop197zs882zl, paid recipient 1 a SECOND time and still never paid
-- recipient 2.
--
-- So resumability cannot live on the KeeperHub side, and this is what makes it
-- ours: only legs that are not yet paid are ever dispatched, and a run as a
-- whole is never re-fired. Without a per-leg record there is nothing to subtract
-- and the only available retry is the one that double-pays.

CREATE TABLE IF NOT EXISTS keeperhub_payout_runs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  hackathon_id UUID NOT NULL REFERENCES hackathons(id) ON DELETE CASCADE,
  pool TEXT NOT NULL CHECK (pool IN ('contributor', 'maintainer')),
  chain_id TEXT NOT NULL REFERENCES chain_configs(chain_id),

  -- What the legs must sum to, frozen when the run is planned.
  --
  -- Held so the sum can be ASSERTED rather than trusted at dispatch time. The
  -- producer already checks its allocation against the pool; repeating the
  -- check here is the same defence in depth settlement.Persist applies, for the
  -- same reason - a Result can be constructed or mutated between computing it
  -- and acting on it.
  pool_minor NUMERIC(30,0) NOT NULL CHECK (pool_minor > 0),

  state TEXT NOT NULL DEFAULT 'planned'
    CHECK (state IN ('planned', 'dispatching', 'complete', 'failed')),

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- One run per event and pool. A second run is a second payment of the same
  -- money, and this is the constraint that makes "never re-fire a run" a
  -- property of the database rather than of whoever is holding the CLI.
  --
  -- Deliberately NOT keyed to a settlements row: writing one would consume
  -- settlements_one_per_event_pool and permanently block the Aptos rail for
  -- this event. The amounts come from hackathon.SettlementFor used as a pure
  -- producer.
  CONSTRAINT keeperhub_payout_runs_one_per_event_pool UNIQUE (hackathon_id, pool)
);

CREATE TABLE IF NOT EXISTS keeperhub_payout_legs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id UUID NOT NULL REFERENCES keeperhub_payout_runs(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id),

  -- FROZEN at plan time, never re-read from contributor_addresses.
  --
  -- The same rule claim_leaves follows: contributor_addresses holds current
  -- intent and changes, while this records where the money was actually sent.
  -- Re-reading it would mean a person who changes their address mid-run is
  -- credited with a payment that went somewhere else.
  --
  -- Mixed case permitted because the stored form is EIP-55 checksummed; see the
  -- unique index below for why uniqueness must nonetheless ignore case.
  address TEXT NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'),
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),

  -- Five states, and 'unknown' is the one that matters.
  --
  -- 'failed' means we know no money moved. 'unknown' means we do not know -
  -- a dispatch that timed out, or an execution we cannot read back. The two
  -- must never be collapsed: retrying a 'failed' leg is correct, and retrying
  -- an 'unknown' one is how the double-payment above happens. An 'unknown' leg
  -- is resolved by reading the chain, by a person, before anything re-sends it.
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'dispatched', 'confirmed', 'failed', 'unknown')),

  execution_id TEXT,
  tx_hash TEXT,
  last_error TEXT,

  dispatched_at TIMESTAMPTZ,
  confirmed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- One leg per person per run. A settlement pays a person once.
  CONSTRAINT keeperhub_payout_legs_one_per_user UNIQUE (run_id, user_id)
);

-- One leg per ADDRESS per run, case-insensitively.
--
-- Separate from the per-user rule and not implied by it: two accounts can name
-- the same destination, and the thing being protected here is the address, not
-- the person. Case-insensitive because EIP-55 gives one address two spellings,
-- so an exact comparison would let the same destination be paid twice in one
-- run - which is the precise failure this whole table exists to prevent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_keeperhub_payout_legs_one_per_address
  ON keeperhub_payout_legs (run_id, lower(address));

-- The dispatch query: everything in this run that has not been paid.
--
-- Partial on exactly the states that may be sent. 'unknown' is excluded
-- deliberately - it must be resolved by a person, never swept up by a retry.
CREATE INDEX IF NOT EXISTS idx_keeperhub_payout_legs_unpaid
  ON keeperhub_payout_legs (run_id, status)
  WHERE status IN ('pending', 'failed');
