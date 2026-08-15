-- Every admin reset of a contributor's KYC status, recorded.
--
-- A rejected verification is terminal in the product: canStartNewKYCSession
-- allows only '', 'expired' and 'not_started', so a contributor whose
-- documents were refused cannot start again from the UI at all. The only exit
-- was an admin running UPDATE by hand against production - which happened, for
-- four contributors, and left no record of who did it, when, or why.
--
-- That is the gap this closes. A reset hands somebody a second attempt at
-- identity verification; it should be as answerable after the fact as a role
-- change is.
--
-- Its own table rather than a column on users: users holds current state, and
-- the question here is historical ("has this person been reset before, and by
-- whom?"). A contributor reset twice is a fact worth being able to see, and a
-- column can only hold the last one.
CREATE TABLE IF NOT EXISTS kyc_reset_audit (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Whose verification was reset.
  subject_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

  -- The state being left behind, captured before the write. Recording only
  -- "reset to expired" would lose the thing worth knowing: whether this was a
  -- refused verification, an abandoned one, or an admin clearing a stuck
  -- pending session. Those are different decisions.
  previous_status TEXT,
  -- The Didit session that was detached. Kept as text rather than dropped,
  -- because it is the key to the decision record on Didit's side if the reset
  -- is ever disputed.
  previous_session_id TEXT,

  -- Who performed it. NOT NULL: a reset always has an actor, unlike the
  -- bootstrap case in admin_role_audit where there was nobody to authorise.
  actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE SET NULL,

  -- Required by the handler, not by the schema, so this table stays writable
  -- by a future backfill that has no reason to record. In normal operation a
  -- reset without a stated reason is not accepted.
  reason TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_kyc_reset_audit_subject
  ON kyc_reset_audit(subject_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_kyc_reset_audit_created
  ON kyc_reset_audit(created_at DESC);
