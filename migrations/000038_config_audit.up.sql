-- Every GrainHack config change and phase transition writes one row here -
-- "why did this contributor get $110" must be answerable months later
-- (AI-specs.md §3.1). key='phase' records a lifecycle transition instead of
-- a settings change; hackathon_id NULL means a global-default change.
CREATE TABLE IF NOT EXISTS config_audit (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  old_value TEXT,
  new_value TEXT,
  actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_config_audit_key ON config_audit(key, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_config_audit_hackathon ON config_audit(hackathon_id, created_at DESC) WHERE hackathon_id IS NOT NULL;
