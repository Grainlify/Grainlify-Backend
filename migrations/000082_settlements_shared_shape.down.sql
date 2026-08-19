DROP INDEX IF EXISTS settlements_one_per_event_pool;
ALTER TABLE settlements DROP CONSTRAINT IF EXISTS settlements_pool_known;
ALTER TABLE settlements DROP COLUMN IF EXISTS pool;
ALTER TABLE settlements DROP COLUMN IF EXISTS chain_id;

ALTER TABLE settlement_lines RENAME CONSTRAINT settlement_lines_user_id_fkey
  TO founding_settlement_lines_user_id_fkey;
ALTER TABLE settlement_lines RENAME CONSTRAINT settlement_lines_settlement_id_fkey
  TO founding_settlement_lines_settlement_id_fkey;
ALTER TABLE settlement_lines RENAME CONSTRAINT settlement_lines_amount_minor_positive
  TO founding_settlement_lines_amount_minor_positive;
ALTER TABLE settlements RENAME CONSTRAINT settlements_total_weight_check
  TO founding_settlements_total_shares_check;
ALTER TABLE settlements RENAME CONSTRAINT settlements_pool_minor_positive
  TO founding_settlements_pool_minor_positive;
ALTER TABLE settlements RENAME CONSTRAINT settlements_hackathon_id_fkey
  TO founding_settlements_hackathon_id_fkey;
ALTER INDEX idx_settlement_lines_settlement
  RENAME TO idx_founding_settlement_lines_settlement;
ALTER INDEX settlement_lines_settlement_id_user_id_key
  RENAME TO founding_settlement_lines_settlement_id_user_id_key;
ALTER INDEX settlement_lines_pkey RENAME TO founding_settlement_lines_pkey;
ALTER INDEX settlements_pkey      RENAME TO founding_settlements_pkey;

ALTER TABLE settlement_lines RENAME COLUMN effective_weight TO effective_shares;
ALTER TABLE settlement_lines RENAME COLUMN raw_weight       TO raw_shares;
ALTER TABLE settlements      RENAME COLUMN unit_value_usdc  TO share_value_usdc;
ALTER TABLE settlements      RENAME COLUMN total_weight     TO total_shares;

ALTER TABLE settlement_lines RENAME TO founding_settlement_lines;
ALTER TABLE settlements      RENAME TO founding_settlements;
