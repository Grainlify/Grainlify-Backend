DROP INDEX IF EXISTS idx_users_kyc_session_id;
DROP INDEX IF EXISTS idx_users_kyc_status;

ALTER TABLE users
  DROP COLUMN IF EXISTS kyc_data,
  DROP COLUMN IF EXISTS kyc_verified_at,
  DROP COLUMN IF EXISTS kyc_session_id,
  DROP COLUMN IF EXISTS kyc_status;
