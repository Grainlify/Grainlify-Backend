-- Restores the column, empty.
--
-- The values are not restorable and are not worth restoring: every one was a
-- Railway CGNAT proxy address. Anything reading this column after a rollback
-- would see NULL, which is the honest answer either way.
ALTER TABLE support_requests ADD COLUMN IF NOT EXISTS reporter_ip TEXT;
