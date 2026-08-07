-- Referral program. Every user gets a stable referral_code (generated lazily
-- on first read of GET /referrals/me, not here - see internal/handlers/referrals.go
-- ensureReferralCode). ref_code on oauth_states carries a referral code
-- through the GitHub OAuth redirect round-trip (mirrors the existing
-- redirect_uri column) so it's available at CallbackUnified when a brand
-- new user row is created.
--
-- referrals is the source of truth for point totals (summed at read time in
-- GetStats) rather than a mutable counter column, so there's no balance to
-- drift out of sync with its own audit trail. A referral starts 'pending' at
-- signup and becomes 'completed' only once the referred user finishes GitHub
-- + KYC verification (internal/handlers/referrals.go maybeCompleteReferral,
-- called from kyc.go Status() and didit_webhook.go Receive()).
ALTER TABLE users ADD COLUMN IF NOT EXISTS referral_code TEXT UNIQUE;
ALTER TABLE oauth_states ADD COLUMN IF NOT EXISTS ref_code TEXT;

CREATE TABLE IF NOT EXISTS referrals (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  referrer_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  referred_user_id UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'pending',
  points_awarded INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_referrals_referrer ON referrals(referrer_user_id);
CREATE INDEX IF NOT EXISTS idx_referrals_referred_pending ON referrals(referred_user_id) WHERE status = 'pending';
