ALTER TABLE support_requests DROP COLUMN IF EXISTS telegram_routed_to_fallback;

DROP INDEX IF EXISTS idx_support_requests_undelivered;
CREATE INDEX IF NOT EXISTS idx_support_requests_undelivered
  ON support_requests(created_at DESC)
  WHERE discord_delivered_at IS NULL
     OR telegram_delivered_at IS NULL
     OR telegram_admin_dm_delivered_at IS NULL;
