DELETE FROM chain_configs WHERE chain_id = 'aptos-testnet';
ALTER TABLE founding_settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_one_reason;
ALTER TABLE founding_settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_excluded_reason_check;
ALTER TABLE founding_settlement_lines DROP COLUMN IF EXISTS excluded_reason;
