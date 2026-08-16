DROP INDEX IF EXISTS idx_social_follow_decisions_reason_code;

ALTER TABLE social_follow_decisions
  DROP CONSTRAINT IF EXISTS social_follow_decisions_reason_code_check;
ALTER TABLE social_follow_submissions
  DROP CONSTRAINT IF EXISTS social_follow_submissions_reason_code_check;

-- Drops the codes and keeps every free-text note, which is the whole reason
-- the two columns were kept separate.
ALTER TABLE social_follow_decisions DROP COLUMN IF EXISTS reason_code;
ALTER TABLE social_follow_submissions DROP COLUMN IF EXISTS reason_code;
