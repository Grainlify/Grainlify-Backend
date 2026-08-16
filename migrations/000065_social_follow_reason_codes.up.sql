-- A rejection reason a machine can read, ALONGSIDE the free-text note.
--
-- decision_reason has always been free text. That is fine for a human reading
-- one decision and useless for anything else: "blurry", "cant see the follow"
-- and "screenshot unreadable" are the same rejection written three ways, so
-- they cannot be counted, filtered, or turned into guidance for the person who
-- has now been rejected twice for the same thing.
--
-- reason_code is added rather than replacing decision_reason, deliberately.
-- Three decisions already carry free-text reasons written by a human. A schema
-- tidy-up that dropped or tried to parse them would destroy the only record of
-- why those contributors were turned down, to make a column look neater.
-- Both columns stay: the code is what we aggregate on, the note is what the
-- person reads.
--
-- Nullable on purpose. Existing rows have no code and never will, and a
-- transitional request from a cached admin bundle can still send a bare note.
-- Anything NULL here means "decided before codes existed, or by a client that
-- did not send one" - it does NOT mean "no reason given".
ALTER TABLE social_follow_submissions
  ADD COLUMN IF NOT EXISTS reason_code TEXT;

ALTER TABLE social_follow_decisions
  ADD COLUMN IF NOT EXISTS reason_code TEXT;

-- The set is closed and enforced here as well as in Go, because this column
-- exists to be aggregated: one typo'd code silently becomes its own category
-- in every count built on it, and nothing would flag it.
--
-- 'other' is in the set and is the one that requires a note - the handler
-- enforces that, since a CHECK cannot see the note column on the decisions
-- table and having half the rule here and half in Go is worse than having it
-- in one place.
ALTER TABLE social_follow_submissions
  DROP CONSTRAINT IF EXISTS social_follow_submissions_reason_code_check;
ALTER TABLE social_follow_submissions
  ADD CONSTRAINT social_follow_submissions_reason_code_check
  CHECK (reason_code IS NULL OR reason_code IN (
    'x_no_follow', 'linkedin_no_follow', 'unreadable', 'wrong_account', 'duplicate', 'other'
  ));

ALTER TABLE social_follow_decisions
  DROP CONSTRAINT IF EXISTS social_follow_decisions_reason_code_check;
ALTER TABLE social_follow_decisions
  ADD CONSTRAINT social_follow_decisions_reason_code_check
  CHECK (reason_code IS NULL OR reason_code IN (
    'x_no_follow', 'linkedin_no_follow', 'unreadable', 'wrong_account', 'duplicate', 'other'
  ));

-- What the codes are for: counting them.
CREATE INDEX IF NOT EXISTS idx_social_follow_decisions_reason_code
  ON social_follow_decisions(reason_code)
  WHERE reason_code IS NOT NULL;
