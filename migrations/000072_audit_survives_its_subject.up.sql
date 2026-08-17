-- An audit row must outlive the people it is about.
--
-- Two defects, both wrong today, both independent of how account deletion
-- eventually works. Found by enumerating every foreign key into users(id)
-- while scoping deletion; neither has ever fired, because nothing has ever
-- deleted a user.
--
--
-- 1. A CONSTRAINT THAT CONTRADICTS ITSELF
--
-- kyc_reset_audit.actor_user_id was declared
--
--     actor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE SET NULL
--
-- which cannot hold. On deletion Postgres attempts to write NULL into a
-- NOT NULL column and raises, so the delete fails with a not-null violation
-- rather than anonymising the actor. The practical effect: any admin who has
-- ever performed a KYC reset is undeletable, and the error explains nothing.
--
-- The NOT NULL was deliberate - "a reset always has an actor" - and it is
-- true at write time. It is the wrong tool for it: the handler requires an
-- actor, and the schema should not also insist on one for ever, because
-- "who did this" and "this happened" have different lifetimes. The record is
-- the thing that must survive; the name attached to it is what may have to go.
--
--
-- 2. THE AUDIT WAS DESTROYED BY THE EVENT IT EXISTED TO RECORD
--
-- Both audit tables CASCADE on their SUBJECT:
--
--     admin_role_audit.subject_user_id  ON DELETE CASCADE
--     kyc_reset_audit.subject_user_id   ON DELETE CASCADE
--
-- so deleting a user destroyed every record of decisions made ABOUT that
-- user - which is exactly backwards. These tables exist for the case where
-- somebody later disputes what was done to them, or where we have to show
-- that a verification was reopened for a stated reason by a named admin.
-- Cascading on the subject means the audit is complete right up until the
-- moment it would first be needed, and the deletion that erases it leaves no
-- trace that anything was erased.
--
-- This is the more serious of the two. The first fails loudly; this one would
-- have succeeded silently.
--
--
-- WHY SET NULL AND NOT RESTRICT
--
-- RESTRICT would preserve the link by making the user undeletable, which
-- turns every audited person into a permanent account. That trades one
-- silent wrong for a loud one and denies a legitimate request on a
-- technicality.
--
-- SET NULL is the floor: whatever else happens, the row survives with its
-- date, its reason, its reason code, its previous status and its snapshot of
-- the decision. It stops being attributable to a named person, which is the
-- part a deletion is entitled to remove.
--
-- Under the tombstone design now chosen for account closure - users row
-- retained, identifying columns nulled, deleted_at set - these clauses never
-- fire at all, because the referenced row is still there and the audit keeps
-- its full linkage. That is the point: this is the behaviour when something
-- bypasses the tombstone (a manual DELETE, a hard erasure request), not the
-- mechanism the product relies on. A floor, not a plan.

-- kyc_reset_audit.actor_user_id: let SET NULL actually work.
ALTER TABLE kyc_reset_audit
  ALTER COLUMN actor_user_id DROP NOT NULL;

-- Both subjects: keep the record, drop the link.
ALTER TABLE kyc_reset_audit
  ALTER COLUMN subject_user_id DROP NOT NULL,
  DROP CONSTRAINT IF EXISTS kyc_reset_audit_subject_user_id_fkey;

ALTER TABLE kyc_reset_audit
  ADD CONSTRAINT kyc_reset_audit_subject_user_id_fkey
  FOREIGN KEY (subject_user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE admin_role_audit
  ALTER COLUMN subject_user_id DROP NOT NULL,
  DROP CONSTRAINT IF EXISTS admin_role_audit_subject_user_id_fkey;

ALTER TABLE admin_role_audit
  ADD CONSTRAINT admin_role_audit_subject_user_id_fkey
  FOREIGN KEY (subject_user_id) REFERENCES users(id) ON DELETE SET NULL;
