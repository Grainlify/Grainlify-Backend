ALTER TABLE keeperhub_dispatch_attempt_legs DROP COLUMN IF EXISTS preflight_raw;
ALTER TABLE keeperhub_dispatch_attempt_legs DROP COLUMN IF EXISTS preflight_checked_at;
ALTER TABLE keeperhub_dispatch_attempt_legs DROP COLUMN IF EXISTS preflight_would_revert;
