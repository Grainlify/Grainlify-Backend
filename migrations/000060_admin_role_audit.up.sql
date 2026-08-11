-- Every change to who holds the admin role, recorded.
--
-- Granting and removing admin are the two actions that decide who can do
-- everything else, and until now they were the only two in the system that
-- left no record at all: SetUserRole was a bare UPDATE, and bootstrap
-- promoted silently. An audit could answer "who changed the draw weights" but
-- not "who made that person an admin" - which is the question the first one
-- depends on.
--
-- Deliberately its own table rather than config_audit: that one is scoped to
-- a hackathon by foreign key and describes event configuration. Role changes
-- are platform-wide and outlive any event.
CREATE TABLE IF NOT EXISTS admin_role_audit (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Whose role changed.
  subject_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

  -- Both sides, not just the new value. "Alice promoted Bob from contributor
  -- to admin" is a different fact from "Alice changed Bob's role", and a
  -- demotion matters as much as a promotion - reading a log of new-values
  -- alone cannot tell you which happened.
  old_role TEXT,
  new_role TEXT NOT NULL,

  -- How the change came about. 'bootstrap' is a self-promotion on a fresh
  -- install with no admin to approve it; 'admin_action' is an existing admin
  -- deciding. A first admin with no origin record is the same gap one step
  -- earlier, so both are recorded.
  source TEXT NOT NULL CHECK (source IN ('admin_action', 'bootstrap')),

  -- Who performed it. For 'bootstrap' this is the same user as the subject,
  -- because there was nobody else to authorise it - which is exactly the
  -- property worth being able to see later.
  actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,

  -- Free-text context: the rejection reason for a refused bootstrap, or an
  -- admin's stated rationale.
  note TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_admin_role_audit_subject
  ON admin_role_audit(subject_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_role_audit_created
  ON admin_role_audit(created_at DESC);

-- Who last changed an event's settings. hackathons already had created_by but
-- nothing recorded an edit, so "who moved the prize pool" had no answer even
-- though "who created the event" did.
--
-- Separate from created_by rather than overwriting it: the two answer
-- different questions, and losing the creator to record an editor would be a
-- net loss of attribution.
ALTER TABLE hackathons ADD COLUMN IF NOT EXISTS updated_by UUID REFERENCES users(id) ON DELETE SET NULL;
