-- A third reason somebody earns an amount and cannot receive it: identity
-- verification is not complete.
--
-- # Why this is a hold and not an exclusion
--
-- KYC is checked once, at wave assignment, and never again. Somebody verified
-- when their wave was assigned and declined afterwards keeps their wave, keeps
-- their shares, and is paid at settlement - see #507. Refusing to pay them is
-- the fix; refusing to pay them PERMANENTLY is not, because we reset people for
-- benign reasons and a settlement running mid-reset would sweep somebody for a
-- cropped photograph.
--
-- Held rather than forfeited, so a benign reset resolves and pays late while a
-- genuine refusal never resolves and never pays. Both correct outcomes fall out
-- of one rule, and the rule does not have to tell them apart.
--
-- # Why the value is added HERE and not in Eligible()
--
-- Adding a KYC check to founding.Eligible would zero the line's effective
-- weight, which removes it from the divisor, which makes Apportion hand that
-- person's money to the other contributors. Permanently: recovering it would
-- mean taking it back from people who did nothing wrong.
--
-- Applied at tree build instead, the person keeps a positive weight and a real
-- allocation, gets no leaf, and their amount lands in residue - and the escrow
-- is funded with the leaf total, so the money never leaves the treasury.
--
-- That is the whole reason this value exists in these two constraints rather
-- than as a fourth branch of Eligible.
ALTER TABLE settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_excluded_reason_check;
ALTER TABLE settlement_lines
  ADD CONSTRAINT founding_settlement_lines_excluded_reason_check
  CHECK (excluded_reason IS NULL OR excluded_reason IN ('no_address', 'no_github_account', 'kyc_unresolved'));

ALTER TABLE settlement_holds DROP CONSTRAINT IF EXISTS settlement_holds_reason_check;
ALTER TABLE settlement_holds
  ADD CONSTRAINT settlement_holds_reason_check
  CHECK (reason IN ('no_address', 'no_github_account', 'kyc_unresolved'));
