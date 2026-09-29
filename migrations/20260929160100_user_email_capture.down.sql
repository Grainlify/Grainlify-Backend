ALTER TABLE users DROP COLUMN IF EXISTS email_declined_at;
ALTER TABLE users DROP COLUMN IF EXISTS email_captured_at;
ALTER TABLE users DROP COLUMN IF EXISTS email_notifications_enabled;
