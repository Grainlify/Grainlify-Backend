DROP INDEX IF EXISTS idx_users_kyc_reconciled_at;
ALTER TABLE users DROP COLUMN IF EXISTS kyc_reconciled_at;
