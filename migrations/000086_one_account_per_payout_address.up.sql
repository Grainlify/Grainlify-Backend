-- One live payout address belongs to one account.
--
-- The existing unique index is (user_id, chain_id) WHERE superseded_at IS NULL:
-- one live address per person per chain. It says nothing about the same address
-- being registered by TWO people, and that is the gap.
--
-- What it allows today: accounts A and B both register address X. A settlement
-- pays X. Both accounts see that claim in /me/claims, because the lookup runs
-- through every address the account has registered. GetClaim refuses rather than
-- returning an arbitrary one - but the account that gets the error is whichever
-- one asks, and that is usually the INNOCENT one, refused a real claim because
-- of something another account did.
--
-- The refusal handles the consequence. This makes the cause impossible.
--
-- Applied now on purpose: production holds ONE live row, so the index applies
-- cleanly. Once thirty-eight people have registered, adding it means first
-- finding and resolving whatever duplicates exist, with real money keyed to
-- them and no non-arbitrary way to pick a winner.
--
-- Plain columns rather than lower(address): contributor_addresses.address is
-- canonicalised on write and carries CHECK (address ~ '^0x[0-9a-f]{64}$'), so
-- the stored form is already lowercase and an expression index would be
-- redundant. If that CHECK is ever relaxed, this index must become
-- lower(address) in the same change.
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributor_addresses_one_account
  ON contributor_addresses (chain_id, address)
  WHERE superseded_at IS NULL;
