-- The record of what somebody was owed and did not receive.
--
-- # Why this must exist before the first publication, and the release need not
--
-- A root is immutable and the residue is computed once. Today the outcome for
-- everyone who earned an amount and got no leaf is computed in
-- payout/resolve.go, aggregated into a Report for the operator to read, and
-- then discarded. We write down who WAS paid - claim_leaves - and never who was
-- owed and was not.
--
-- Reconstructing it afterwards means re-running the resolve against
-- contributor_addresses and github_accounts as they are THEN. Somebody
-- registering an address the day after publication silently changes the answer
-- to "who was excluded from tree N". The record is only true at the moment the
-- root becomes immutable, so that is the moment it has to be written.
--
-- The machinery that releases a hold can arrive later. This cannot.
--
-- # Why a table and not columns on settlement_lines
--
-- settlement_lines is the record of what tree N said, and tree N is immutable.
-- Putting release state there would mean settlement N+1's publication MUTATES
-- settlement N's record - editing the account of an event that already
-- happened, in the field most likely to be read by somebody reconstructing what
-- was true at the time.
--
-- One is history, one is live state. settlement_lines.excluded_reason stays the
-- historical record; this is the object that moves.
CREATE TABLE IF NOT EXISTS settlement_holds (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id              UUID NOT NULL REFERENCES users(id),
  origin_settlement_id UUID NOT NULL REFERENCES settlements(id),

  -- Frozen at creation and never recomputed.
  --
  -- The opposite treatment to `reason` below, deliberately: an amount
  -- recomputed later is computed against inputs that have moved - a different
  -- pool, a different divisor, a different set of eligible people - so it would
  -- silently stop being the number this person actually earned in tree N.
  amount_minor         BIGINT NOT NULL CHECK (amount_minor > 0),

  -- EXPLANATION ONLY. This must never gate a release.
  --
  -- The opposite treatment to `amount_minor` above, and for the opposite
  -- reason: releasing on "the stored reason is now resolved" pays exactly the
  -- person a later hold exists to stop. Somebody held as 'no_address' can
  -- register an address and still be refused identity verification. Release
  -- runs the FULL eligibility check at release time, against facts as they are
  -- then; freezing eligibility would pay on stale ones.
  --
  -- Amount frozen because recomputation goes wrong once inputs move.
  -- Eligibility recomputed because freezing it would pay on stale facts.
  -- Same row, opposite rules, opposite reasons.
  reason               TEXT NOT NULL CHECK (reason IN ('no_address', 'no_github_account')),

  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Which settlement consumed this hold, not merely that something did.
  --
  -- A flag would not survive a retried publication: "already released" cannot
  -- be told from "released into an earlier tree", so a failed-then-retried
  -- build of tree N+1 either loses the hold or pays it twice.
  released_in_settlement_id UUID REFERENCES settlements(id),
  released_at               TIMESTAMPTZ,

  -- Released is one fact in two columns; neither is meaningful alone.
  CONSTRAINT settlement_holds_release_is_atomic CHECK (
    (released_in_settlement_id IS NULL AND released_at IS NULL) OR
    (released_in_settlement_id IS NOT NULL AND released_at IS NOT NULL)
  ),

  -- A retried publication of tree N must not create a second hold for the same
  -- person. Build is idempotent on the root and the leaves; this makes it
  -- idempotent on the ledger too.
  CONSTRAINT settlement_holds_one_per_origin UNIQUE (user_id, origin_settlement_id)
);

-- The query release will run: everything still held, oldest first.
CREATE INDEX IF NOT EXISTS idx_settlement_holds_unreleased
  ON settlement_holds (user_id, created_at)
  WHERE released_in_settlement_id IS NULL;
