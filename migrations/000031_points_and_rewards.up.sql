-- Unified points ledger: every point-earning or point-spending event across
-- all reward programs (referrals, social-follow, future sources) writes one
-- row here. A user's spendable balance is SUM(amount) at read time - no
-- mutable balance column to drift out of sync with its own history.
CREATE TABLE IF NOT EXISTS point_ledger (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount INT NOT NULL, -- positive = earned, negative = redeemed/reversed
  reason TEXT NOT NULL, -- 'referral' | 'social_follow' | 'redemption' | 'redemption_reversal'
  reference_id UUID, -- id of the referrals/social_follow_completions/redemptions row that caused this entry
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_point_ledger_user ON point_ledger(user_id);

-- Backfill: give existing completed referrals their ledger entries so
-- balances computed from the ledger match what /referrals/me already showed
-- (referrals.points_awarded stays as-is; nothing here changes that table).
INSERT INTO point_ledger (user_id, amount, reason, reference_id, created_at)
SELECT referrer_user_id, points_awarded, 'referral', id, completed_at
FROM referrals
WHERE status = 'completed' AND points_awarded > 0;

-- Social-follow program: one row per (user, platform) proof submission.
-- Screenshot is stored as a base64 data URL string, same convention as
-- ecosystems.logo_url - no new file-storage infrastructure needed.
-- Resubmission (e.g. after rejection) upserts this same row rather than
-- creating a new one, so there's only ever one current proof per platform.
CREATE TABLE IF NOT EXISTS social_follow_submissions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  platform TEXT NOT NULL,
  screenshot TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'approved' | 'rejected'
  rejection_reason TEXT,
  reviewed_by UUID REFERENCES users(id),
  reviewed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, platform)
);
CREATE INDEX IF NOT EXISTS idx_social_follow_submissions_status ON social_follow_submissions(status);

-- Fires once, when a user has an approved submission for every platform in
-- socialFollowPlatforms (internal/handlers/social_follow.go). PRIMARY KEY on
-- user_id makes the completion itself the guard against double-awarding.
CREATE TABLE IF NOT EXISTS social_follow_completions (
  user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  points_awarded INT NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Redemption requests: points -> USDC. Points are deducted (a negative
-- point_ledger row) at request time, not at payout time, so the same points
-- can't be spent on two simultaneous requests. A rejected request reverses
-- the deduction with a positive 'redemption_reversal' entry. Actually
-- sending USDC is a manual, off-system step for now (admin marks paid after
-- sending it themselves) - see internal/handlers/redemptions.go for why.
CREATE TABLE IF NOT EXISTS redemptions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  points_spent INT NOT NULL,
  usdc_amount NUMERIC(18,6) NOT NULL,
  stellar_wallet_address TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'paid' | 'rejected'
  admin_note TEXT,
  reviewed_by UUID REFERENCES users(id),
  reviewed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_redemptions_user ON redemptions(user_id);
CREATE INDEX IF NOT EXISTS idx_redemptions_status ON redemptions(status);
