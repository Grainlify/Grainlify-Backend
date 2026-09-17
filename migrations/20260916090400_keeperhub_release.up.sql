-- What releasing a KeeperHub payout run needs to record.
--
-- Everything here serves one rule: only unpaid legs are ever dispatched, and a
-- leg that may have paid is never re-sent until somebody has established that it
-- did not. That rule is only enforceable if every dispatch ATTEMPT is recorded -
-- which legs it carried, in which order, under which idempotency key - before
-- the request leaves this process.

-- 1. A KeeperHub run pays a specific computed payout run.
--
-- GuardPayoutRelease is asked about a hackathon_payout_runs row, so the run it
-- approved has to be the run that is paid. Without this link a release could be
-- approved against one computation and paid from another.
--
-- NOT NULL is safe to add: nothing has ever written keeperhub_payout_runs.
ALTER TABLE keeperhub_payout_runs
  ADD COLUMN IF NOT EXISTS hackathon_payout_run_id UUID NOT NULL
    REFERENCES hackathon_payout_runs(id);
ALTER TABLE keeperhub_payout_runs
  ADD COLUMN IF NOT EXISTS released_by UUID REFERENCES users(id) ON DELETE SET NULL;

-- 2. One row per dispatch attempt.
--
-- A resume is a NEW attempt with a NEW idempotency key, never a re-send of an
-- old one - KeeperHub dedupes on the key and would replay the old execution.
-- So attempts are rows, and the key is unique across them.
CREATE TABLE IF NOT EXISTS keeperhub_dispatch_attempts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id UUID NOT NULL REFERENCES keeperhub_payout_runs(id) ON DELETE CASCADE,

  -- NULL only while the request is being sent. Recorded even when the call
  -- fails, because a request that timed out may still have been accepted, and
  -- the key is the one handle anybody has on it afterwards.
  idempotency_key TEXT UNIQUE,
  -- NULL when KeeperHub never acknowledged. Unique: two attempts claiming one
  -- execution is precisely the replay the fresh key exists to prevent.
  execution_id TEXT UNIQUE,

  -- sending        written BEFORE the request leaves; its legs are already
  --                marked dispatched, so a crash mid-call cannot leave them
  --                looking unpaid and eligible for a second send
  -- sent           KeeperHub acknowledged with an execution id - acceptance,
  --                not payment
  -- unacknowledged the call failed or returned no execution id; its legs are
  --                unknown, because the run may be executing anyway
  -- reconciled     per-leg results were read back and written
  -- mismatch       the execution's recorded input was not what we sent; its
  --                legs are unknown
  state TEXT NOT NULL DEFAULT 'sending'
    CHECK (state IN ('sending', 'sent', 'unacknowledged', 'reconciled', 'mismatch')),
  error TEXT,
  actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  reconciled_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_keeperhub_dispatch_attempts_run
  ON keeperhub_dispatch_attempts (run_id, created_at);

-- 3. Which legs an attempt carried, and in what order.
--
-- The order is load-bearing: KeeperHub iterates the list in order, so iteration
-- i of an execution belongs to position i of its attempt. Reconciling without it
-- would mean matching on address, and two legs can share one.
CREATE TABLE IF NOT EXISTS keeperhub_dispatch_attempt_legs (
  attempt_id UUID NOT NULL REFERENCES keeperhub_dispatch_attempts(id) ON DELETE CASCADE,
  leg_id UUID NOT NULL REFERENCES keeperhub_payout_legs(id) ON DELETE CASCADE,
  position INT NOT NULL CHECK (position >= 0),
  PRIMARY KEY (attempt_id, position),
  CONSTRAINT keeperhub_dispatch_attempt_legs_once UNIQUE (attempt_id, leg_id)
);

-- 4. A leg knows its latest attempt, and who resolved it by hand if anybody did.
--
-- Intake only writes a result onto a leg whose latest attempt is the one being
-- reconciled. Without that, reading an old attempt late would overwrite what a
-- newer one established.
ALTER TABLE keeperhub_payout_legs
  ADD COLUMN IF NOT EXISTS last_attempt_id UUID REFERENCES keeperhub_dispatch_attempts(id);
ALTER TABLE keeperhub_payout_legs
  ADD COLUMN IF NOT EXISTS resolved_by UUID REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE keeperhub_payout_legs
  ADD COLUMN IF NOT EXISTS resolution_note TEXT;

-- A confirmed leg carries the transaction that confirmed it. A confirmation with
-- nothing to check it against is a claim, and this table records facts.
ALTER TABLE keeperhub_payout_legs DROP CONSTRAINT IF EXISTS keeperhub_payout_legs_confirmed_has_tx;
ALTER TABLE keeperhub_payout_legs
  ADD CONSTRAINT keeperhub_payout_legs_confirmed_has_tx
  CHECK (status <> 'confirmed' OR (tx_hash IS NOT NULL AND tx_hash <> ''));

-- 5. Who earned an amount and was not made a leg, and why.
--
-- Excluded, recorded, and never silently dropped. Written at plan time, when the
-- run is frozen, for the reason settlement_holds gives: this is only true at the
-- moment the legs are fixed, and recomputing it later against addresses and KYC
-- as they are THEN answers a different question.
CREATE TABLE IF NOT EXISTS keeperhub_payout_exclusions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id UUID NOT NULL REFERENCES keeperhub_payout_runs(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id),
  amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
  -- The same three reasons, in the same order, that payout.Resolve applies to
  -- the Aptos rail: a person must not be payable on one rail and held on the
  -- other for the same event.
  reason TEXT NOT NULL CHECK (reason IN ('no_github_account', 'kyc_unresolved', 'no_address')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT keeperhub_payout_exclusions_once UNIQUE (run_id, user_id)
);
