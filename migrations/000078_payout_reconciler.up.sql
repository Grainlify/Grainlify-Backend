-- The reconciler's alert dedupe, and a category that routes to the admin DM.

-- WHY A NEW CATEGORY RATHER THAN REUSING 'kyc'.
--
-- Only 'kyc' currently reaches the admin DM; everything else lands in a public
-- topic. A reconciler alert names a leaf hash, an address, an amount and a
-- contributor, which is at least as sensitive as a KYC notice and must not be
-- posted anywhere joinable.
--
-- Reusing 'kyc' would route correctly and make two unrelated things
-- indistinguishable in the one place somebody reads them under pressure - at
-- 3am, deciding whether a payout is broken or somebody needs their ID checked.
ALTER TABLE support_requests DROP CONSTRAINT IF EXISTS support_requests_category_check;
ALTER TABLE support_requests
  ADD CONSTRAINT support_requests_category_check
  CHECK (category IN ('bug', 'kyc', 'idea', 'help', 'other', 'payout'));

-- One alert per disagreement, not one per poll.
--
-- Same shape as kyc_review_alerts and for the same reason: the reconciler runs
-- on a timer, so without a claim row it would re-send every cycle. An alert that
-- repeats every thirty seconds is an alert that gets muted, and a muted alert is
-- worse than none because it still looks like coverage.
--
-- The key is (kind, subject) rather than a row id: the same disagreement should
-- dedupe even if it is noticed through a different operation row.
CREATE TABLE IF NOT EXISTS chain_reconcile_alerts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  kind TEXT NOT NULL CHECK (kind IN ('paid_but_unclaimed', 'root_mismatch')),
  -- Leaf hash for paid_but_unclaimed, event ref for root_mismatch. Hex, so one
  -- column serves both and the unique index is simple.
  subject TEXT NOT NULL,
  event_ref UUID,
  detail TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Set by hand once somebody has dealt with it. Deliberately not settable by
  -- the reconciler: trap 10 - where a silent reconciliation would be wrong and
  -- nobody is present to consult, the machine must not pick a direction.
  resolved_at TIMESTAMPTZ,
  resolved_note TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_reconcile_alerts_unique
  ON chain_reconcile_alerts (kind, subject);

-- What we published, per event, so the reconciler can check the chain against a
-- recorded intention rather than against nothing.
--
-- Without this the reconciler can settle leaves but cannot detect the worst
-- disagreement there is: a published root that is not the root we built our
-- proofs from. That case means every proof being served is invalid, and it is
-- only visible by comparing two values - so both have to exist.
--
-- escrow_address is recorded rather than derived, even though it is
-- deterministic from (admin, event_id). Deriving it at read time would make the
-- reconciler agree with itself: if the address is wrong because the event id was
-- wrong, recomputing produces the same wrong address and the mismatch never
-- surfaces. Storing what we actually used lets the two disagree.
CREATE TABLE IF NOT EXISTS payout_event_roots (
  settlement_id UUID PRIMARY KEY REFERENCES founding_settlements(id) ON DELETE RESTRICT,
  chain_id TEXT NOT NULL,
  escrow_address TEXT NOT NULL,
  root BYTEA NOT NULL CHECK (octet_length(root) = 32),
  -- The leaf total, which publish_root requires to equal what was funded.
  total_minor BIGINT NOT NULL CHECK (total_minor > 0),
  leaf_count INT NOT NULL CHECK (leaf_count > 0),
  published_tx TEXT,
  published_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
