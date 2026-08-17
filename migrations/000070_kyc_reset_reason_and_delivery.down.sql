DROP INDEX IF EXISTS idx_kyc_reset_audit_unnotified;

ALTER TABLE kyc_reset_audit
  DROP COLUMN IF EXISTS reason_code,
  DROP COLUMN IF EXISTS note,
  DROP COLUMN IF EXISTS previous_kyc_data,
  DROP COLUMN IF EXISTS notified_at,
  DROP COLUMN IF EXISTS notify_error;
