-- Platform fee, taken once off the sponsor's total before anything is split.
--
--   sponsor_total_usdc
--     - platform_fee_usdc          (sponsor_total x fee rate, taken once)
--     = net                        (what actually pays people)
--         -> maintainer_prize_pool (net x maintainer share)
--         -> contributor_prize_pool (the remainder)
--
-- **contributor_prize_pool and maintainer_prize_pool now hold NET amounts.**
-- That is the whole design: every existing reader of those columns - the
-- contributor divisor, the maintainer allocation, the payout floor, the
-- public API, the per-chain arithmetic - keeps working unchanged and is
-- correct by construction, rather than correct because every call site was
-- found and edited. Fourteen readers converted by hand would be fourteen
-- chances to miss one, and a reader added next year would not know.
--
-- The fee is therefore never subtracted anywhere downstream. If a future
-- change makes a payout path read sponsor_total_usdc, it is paying out the
-- fee - which is what the guard test in internal/hackathon exists to catch.

ALTER TABLE hackathons
  -- What the sponsor actually put in. Nullable: events created before this
  -- migration have no recorded total, and inventing one by adding the pools
  -- back together would assert a fee of zero that nobody agreed to. Production
  -- holds zero hackathons, so in practice this is null for nothing.
  ADD COLUMN IF NOT EXISTS sponsor_total_usdc NUMERIC(18,6),
  -- The fee taken, in money rather than as a rate, so the row stays readable
  -- after the configured rate changes.
  ADD COLUMN IF NOT EXISTS platform_fee_usdc NUMERIC(18,6),
  -- The rate that produced it, snapshotted at the moment it was applied. Kept
  -- alongside the amount because "5% of 10,000" and "500" answer different
  -- questions during a dispute, and the config it came from is mutable.
  ADD COLUMN IF NOT EXISTS platform_fee_rate_pct NUMERIC(6,3),
  -- The share of net allocated to maintainers, also snapshotted, so the split
  -- can be re-derived from the row alone.
  ADD COLUMN IF NOT EXISTS maintainer_share_pct NUMERIC(6,3);

-- The three numbers must always reconcile. Enforced in the database rather
-- than only in the handler, because a fee that silently fails to add up is
-- indistinguishable from a skim.
--
-- Written to tolerate nulls (pre-fee events) and to allow the 0.01 of
-- rounding slack that money arithmetic needs: the fee and the maintainer
-- share are rounded, and the contributor pool takes the remainder so the
-- parts sum exactly - this constraint is the backstop, not the mechanism.
ALTER TABLE hackathons
  ADD CONSTRAINT hackathons_fee_reconciles CHECK (
    sponsor_total_usdc IS NULL
    OR abs(
         sponsor_total_usdc
         - COALESCE(platform_fee_usdc, 0)
         - COALESCE(contributor_prize_pool, 0)
         - COALESCE(maintainer_prize_pool, 0)
       ) <= 0.01
  );

-- A payout run records what it divided, so it can never be ambiguous later
-- whether a figure was gross or net. contributor_prize_pool on this table
-- already holds what was divided and is therefore net; these two make the
-- other side of the arithmetic explicit rather than inferred.
ALTER TABLE hackathon_payout_runs
  ADD COLUMN IF NOT EXISTS sponsor_total_usdc NUMERIC(18,6),
  ADD COLUMN IF NOT EXISTS platform_fee_usdc NUMERIC(18,6);
