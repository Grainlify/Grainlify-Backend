-- Remove csrf_cookie column from oauth_states table.
ALTER TABLE oauth_states
  DROP COLUMN IF EXISTS csrf_cookie;
