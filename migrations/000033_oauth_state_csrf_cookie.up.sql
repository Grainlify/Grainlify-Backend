-- Add csrf_cookie column to oauth_states table.
-- Binds a LoginStart-issued state to the browser that requested it via a
-- second, HttpOnly cookie value, closing the login-CSRF gap described in
-- docs/security/oauth-csrf-protection.md's "Session fixation" out-of-scope note.
ALTER TABLE oauth_states
  ADD COLUMN IF NOT EXISTS csrf_cookie TEXT;
