ALTER TABLE hackathon_payout_runs
  DROP COLUMN IF EXISTS platform_fee_usdc,
  DROP COLUMN IF EXISTS sponsor_total_usdc;

ALTER TABLE hackathons DROP CONSTRAINT IF EXISTS hackathons_fee_reconciles;

-- The pool columns are left holding NET amounts. Reversing that would mean
-- adding the fee back into the contributor pool, which would assert the fee
-- was never taken - a claim this migration cannot make on the sponsor's
-- behalf.
ALTER TABLE hackathons
  DROP COLUMN IF EXISTS maintainer_share_pct,
  DROP COLUMN IF EXISTS platform_fee_rate_pct,
  DROP COLUMN IF EXISTS platform_fee_usdc,
  DROP COLUMN IF EXISTS sponsor_total_usdc;
