-- Which version of the Terms each person accepted, and when.
--
-- Until now acceptance was a flag in the browser's localStorage: it said
-- "somebody on this device pressed Accept, at some point, on some text". It
-- could not say which text, it did not follow the person to another device,
-- and nothing on the server could ask whether anybody had seen a change. So a
-- material change to the Terms reached nobody who did not happen to reopen
-- the page.
--
-- One row per acceptance rather than a column on users, so accepting a new
-- version adds to the record instead of overwriting the evidence of the old
-- one. "What had this person agreed to on the day of the payout?" is a
-- question the history answers and a single column cannot.
--
-- The version is the string the frontend shows (a date, e.g. 2026-10-03); the
-- backend checks it against the versions it knows (internal/terms) so a
-- client cannot record agreement to a text that was never published.
--
-- Nothing else is stored: no IP address, no user agent. They would make the
-- record look more like evidence and are personal data we would then have to
-- justify keeping.
CREATE TABLE IF NOT EXISTS terms_acceptances (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  version      TEXT NOT NULL,
  accepted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Accepting the same version twice (two tabs, a double click) is one
  -- acceptance, not two. The first timestamp is the one that counts.
  UNIQUE (user_id, version)
);

CREATE INDEX IF NOT EXISTS terms_acceptances_user_latest
  ON terms_acceptances (user_id, accepted_at DESC);
