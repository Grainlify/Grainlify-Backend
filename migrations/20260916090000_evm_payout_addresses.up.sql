-- Payout addresses stop being Aptos-shaped by assumption. Grainlify-Backend#548.
--
-- # The gap this closes
--
-- chain_id is free text checked only for being non-empty, and the address CHECK
-- encodes ONE chain's format. So an Aptos address registers cleanly under
-- chain_id 'base' today, and it is exactly the row an EVM payout path would
-- read. The chain and the address shape are two facts that must agree, and
-- nothing made them agree.
--
-- The companion half of #548 - payoutaddr.Validate left-padding a 40-hex EVM
-- address into a DIFFERENT, valid Aptos address - is fixed in Go, because it is
-- a choice of validator rather than a constraint. This migration is what makes
-- the database refuse the result if that choice is ever made wrongly again.

-- 1. A chain declares its family.
--
-- Nullable-then-NOT NULL rather than a DEFAULT: a default would silently call
-- the next chain somebody seeds 'aptos', and the whole failure being fixed here
-- is a chain whose family was assumed instead of stated. With no default, a
-- seed that omits it fails loudly at INSERT - see the Base row that follows in
-- its own migration.
ALTER TABLE chain_configs ADD COLUMN IF NOT EXISTS family TEXT;

UPDATE chain_configs SET family = 'aptos' WHERE chain_id LIKE 'aptos%' AND family IS NULL;

ALTER TABLE chain_configs DROP CONSTRAINT IF EXISTS chain_configs_family_known;
ALTER TABLE chain_configs
  ADD CONSTRAINT chain_configs_family_known CHECK (family IN ('aptos', 'evm'));

ALTER TABLE chain_configs ALTER COLUMN family SET NOT NULL;

-- 2. An address declares the family it is shaped for.
--
-- Carried on the row rather than joined from chain_configs because a CHECK
-- cannot reference another table. That is not a workaround: the address shape
-- and the family have to be verifiable together, in one constraint, at write
-- time, and a join would make the guarantee depend on chain_configs never being
-- edited afterwards.
ALTER TABLE contributor_addresses ADD COLUMN IF NOT EXISTS chain_family TEXT;

UPDATE contributor_addresses SET chain_family = 'aptos' WHERE chain_family IS NULL;

ALTER TABLE contributor_addresses ALTER COLUMN chain_family SET NOT NULL;

ALTER TABLE contributor_addresses DROP CONSTRAINT IF EXISTS contributor_addresses_chain_family_known;
ALTER TABLE contributor_addresses
  ADD CONSTRAINT contributor_addresses_chain_family_known
  CHECK (chain_family IN ('aptos', 'evm'));

-- 3. The per-chain format check, replacing the single-chain one.
--
-- The old constraint was CHECK (address ~ '^0x[0-9a-f]{64}$') - correct for
-- Aptos and the reason nothing has been corrupted yet, but it is also what
-- would reject every EVM address, so it cannot simply stay.
--
-- Aptos: 64 lowercase hex, canonicalised on write, unchanged from before.
-- EVM: exactly 40 hex, and MIXED CASE IS PERMITTED ON PURPOSE - the stored form
-- is the EIP-55 checksummed spelling, so the row carries its own integrity
-- check and a corrupted address is detectable later rather than being 40
-- plausible characters. Never 64, and never padded to it.
ALTER TABLE contributor_addresses DROP CONSTRAINT IF EXISTS contributor_addresses_address_check;
ALTER TABLE contributor_addresses
  ADD CONSTRAINT contributor_addresses_address_matches_family CHECK (
    (chain_family = 'aptos' AND address ~ '^0x[0-9a-f]{64}$') OR
    (chain_family = 'evm'   AND address ~ '^0x[0-9a-fA-F]{40}$')
  );

-- 4. Uniqueness becomes case-insensitive, as migration 000086 asked in writing.
--
-- That migration chose plain columns over lower(address) and said why: the
-- address CHECK forced lowercase, so an expression index would be redundant.
-- It also left an instruction - "If that CHECK is ever relaxed, this index must
-- become lower(address) in the same change." Step 3 is that relaxation, so this
-- is that change.
--
-- It is load-bearing, not tidiness. EIP-55 means one EVM address has two
-- spellings that differ only in case; without lower(), the same address
-- registers twice to two accounts, which is precisely the defect 000086 exists
-- to make impossible.
DROP INDEX IF EXISTS idx_contributor_addresses_one_account;
CREATE UNIQUE INDEX IF NOT EXISTS idx_contributor_addresses_one_account
  ON contributor_addresses (chain_id, lower(address))
  WHERE superseded_at IS NULL;

DROP INDEX IF EXISTS idx_contributor_addresses_lookup;
CREATE INDEX IF NOT EXISTS idx_contributor_addresses_lookup
  ON contributor_addresses (chain_id, lower(address)) WHERE superseded_at IS NULL;
