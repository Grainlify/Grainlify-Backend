-- Irreversible by design.
--
-- The up migration repaired rows corrupted by a sync bug. It did not record
-- which rows it touched, and it could not usefully do so: restoring
-- merged = FALSE on a pull request that is genuinely merged would put the
-- corruption back. Rolling this migration back is a no-op on purpose.
SELECT 1;
