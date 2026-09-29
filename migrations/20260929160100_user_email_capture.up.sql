-- Storing a contributor's email address so notification emails can be sent.
--
-- users.email has existed since 000029 and nothing outside tests has ever
-- written to it: the GitHub OAuth flow requests user:email, reads the primary
-- address at login, returns it in the /me response and drops it. So every
-- notification email the product has ever tried to send was skipped for want
-- of an address, silently, for every user.
--
-- Three columns rather than one, because an address you cannot refuse and
-- cannot remove is not one somebody gave you.

-- The master switch. Separate from notification_preferences.email, which is
-- per-type: this one means "no email from Grainlify at all", and in-app
-- notifications carry on regardless.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_notifications_enabled BOOLEAN NOT NULL DEFAULT true;

-- When the address on file was last written. Answers "how old is this?" for
-- an address captured at a login months ago, and is what the settings screen
-- shows beside it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_captured_at TIMESTAMPTZ;

-- Set when somebody removes their stored address. While it is set, login must
-- not capture the address again.
--
-- Without this, "remove my email" is a button that undoes itself the next time
-- they sign in - the removal would look like it worked, and the address would
-- be back within a day with nothing said. That is worse than having no button.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_declined_at TIMESTAMPTZ;
