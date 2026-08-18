-- Store the settlement in exact integer minor units.
--
-- A Merkle leaf commits to an exact integer, and a published root cannot be
-- corrected. The settlement therefore has to record the integer it committed
-- to, not a decimal rendering of it that arithmetic has to reconstruct.
--
-- WHY THE DECIMAL COLUMNS ARE NOT ENOUGH.
--
-- usdc_amount is NUMERIC(18,6), which can express any USDC amount exactly, so
-- on its own it would round-trip. The problem is that reconstructing the leaf
-- means multiplying it by 10^6 and trusting that every reader does so the same
-- way. Storing amount_minor removes the reconstruction step entirely: the
-- number in the leaf is the number in the row.
--
-- effective_shares is a genuine loss, not a stylistic one. It is
-- NUMERIC(12,4), but effective shares are raw_shares (4 dp) x multiplier
-- (3 dp) and therefore carry 7 decimal places. Every settlement line written
-- under the old width would have silently rounded the one figure that explains
-- how the amount was derived - so a member recomputing their own payout from
-- their own row could not arrive at what they were paid. Widened to (19,7),
-- which is exact for that product.
--
-- Safe to apply as a plain widening: founding.Compute had no production caller
-- and no settlement has ever been recorded, so both tables are empty. The
-- defaults exist for that reason and not because a backfill is expected.

ALTER TABLE founding_settlements
  ADD COLUMN IF NOT EXISTS pool_minor BIGINT NOT NULL DEFAULT 0,
  -- Carried with the value so an amount is never ambiguous about what it
  -- means, matching chain.Amount. A settlement read years later must not
  -- depend on 6 still being the assumed precision.
  ADD COLUMN IF NOT EXISTS asset_decimals INT NOT NULL DEFAULT 6;

ALTER TABLE founding_settlements
  ADD CONSTRAINT founding_settlements_pool_minor_positive
  CHECK (pool_minor >= 0);

ALTER TABLE founding_settlement_lines
  ADD COLUMN IF NOT EXISTS amount_minor BIGINT NOT NULL DEFAULT 0;

ALTER TABLE founding_settlement_lines
  ADD CONSTRAINT founding_settlement_lines_amount_minor_positive
  CHECK (amount_minor >= 0);

-- 7 dp, because raw_shares (4) x multiplier (3) is exactly 7. Widening only;
-- no existing value can fail to fit.
ALTER TABLE founding_settlement_lines
  ALTER COLUMN effective_shares TYPE NUMERIC(19,7);
