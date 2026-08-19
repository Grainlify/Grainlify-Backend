-- Narrowing effective_shares back to (12,4) would round any recorded value,
-- so this reverses only what can be reversed without losing precision.

ALTER TABLE founding_settlement_lines
  DROP CONSTRAINT IF EXISTS founding_settlement_lines_amount_minor_positive;
ALTER TABLE founding_settlement_lines
  DROP COLUMN IF EXISTS amount_minor;

ALTER TABLE founding_settlements
  DROP CONSTRAINT IF EXISTS founding_settlements_pool_minor_positive;
ALTER TABLE founding_settlements
  DROP COLUMN IF EXISTS pool_minor,
  DROP COLUMN IF EXISTS asset_decimals;

ALTER TABLE founding_settlement_lines
  ALTER COLUMN effective_shares TYPE NUMERIC(19,7);
