ALTER TABLE oauth_states
  DROP CONSTRAINT IF EXISTS oauth_states_kind_check;

ALTER TABLE oauth_states
  ADD CONSTRAINT oauth_states_kind_check CHECK (kind IN ('github_link'));

ALTER TABLE oauth_states
  ALTER COLUMN user_id SET NOT NULL;
