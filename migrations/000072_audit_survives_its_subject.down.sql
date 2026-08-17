-- Restores the cascades, and therefore restores the defect.
--
-- Kept honest rather than made a no-op: this one CAN be reversed, unlike a
-- backfill that burns a sequence number, and a down migration that silently
-- did nothing would be its own lie.
--
-- It will fail if any audit row has already had its subject or actor nulled
-- by a real deletion - NOT NULL cannot be restored over existing NULLs. That
-- failure is correct and worth stating: at that point the rows it would break
-- are precisely the records this migration was written to preserve, and
-- reversing it would delete them.
ALTER TABLE admin_role_audit
  DROP CONSTRAINT IF EXISTS admin_role_audit_subject_user_id_fkey;
ALTER TABLE admin_role_audit
  ADD CONSTRAINT admin_role_audit_subject_user_id_fkey
  FOREIGN KEY (subject_user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE admin_role_audit
  ALTER COLUMN subject_user_id SET NOT NULL;

ALTER TABLE kyc_reset_audit
  DROP CONSTRAINT IF EXISTS kyc_reset_audit_subject_user_id_fkey;
ALTER TABLE kyc_reset_audit
  ADD CONSTRAINT kyc_reset_audit_subject_user_id_fkey
  FOREIGN KEY (subject_user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE kyc_reset_audit
  ALTER COLUMN subject_user_id SET NOT NULL;

ALTER TABLE kyc_reset_audit
  ALTER COLUMN actor_user_id SET NOT NULL;
