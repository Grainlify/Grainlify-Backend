-- Account deletion: the request, what was done, and what was kept.
--
-- The Terms have promised deletion on request since they were written, and
-- nothing could carry it out: no endpoint, no procedure, and a schema in which
-- a plain DELETE FROM users either fails (keeperhub_payout_legs,
-- settlement_holds and projects refuse it) or cascades through settlement_lines
-- and redemptions, destroying the record of money already paid.
--
-- So an account is erased in place, not deleted. The users row stays as an
-- empty tombstone, so every payout record that points at it keeps a valid
-- reference; everything that identifies the person is removed from it and from
-- the tables around it (internal/erasure). erased_at marks the tombstone, and
-- is what ends access: a token issued before the erasure is refused from then
-- on (internal/handlers/account_deletion.go), and nothing can sign in to it
-- again, because the GitHub link and every sign-in wallet are gone.
ALTER TABLE users ADD COLUMN IF NOT EXISTS erased_at TIMESTAMPTZ;

-- One row per request. Kept after the erasure, because "what did you delete,
-- what did you keep and why" is a question the person (or a regulator) can ask
-- afterwards, and this is the answer. It holds no personal data of its own:
-- the user id points at the tombstone, and the erased/retained summaries are
-- counts and reasons, never values.
--
-- user_id is SET NULL rather than CASCADE for the same reason kyc_reset_audit's
-- subject is: a record of a decision about somebody must outlive them, even if
-- somebody later bypasses this path with a raw DELETE.
CREATE TABLE IF NOT EXISTS account_deletion_requests (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
  -- scheduled: waiting for the grace period to end; cancellable.
  -- held:      the grace period is over but money is still on its way to this
  --            person, so erasing their payout details now would strand it.
  --            Re-checked on every pass; proceeds by itself once it clears.
  -- cancelled: the person withdrew it during the grace period.
  -- completed: erased.
  status          TEXT NOT NULL DEFAULT 'scheduled'
                  CHECK (status IN ('scheduled', 'held', 'cancelled', 'completed')),
  requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  execute_after   TIMESTAMPTZ NOT NULL,
  cancelled_at    TIMESTAMPTZ,
  completed_at    TIMESTAMPTZ,
  -- Why it is held, in words the settings screen shows as-is.
  hold_reason     TEXT,
  -- Retry bookkeeping for the steps that talk to other services (GitHub,
  -- Didit, the bounty agent). A failure there defers the erasure rather than
  -- skipping the step, up to a limit; see internal/erasure.
  attempts        INT NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ,
  last_error      TEXT,
  -- What was erased, per table, as row counts. Never the values.
  erased          JSONB,
  -- What was kept, why, and until when: [{what, why, until}].
  retained        JSONB,
  -- The outcome of each step at another service: {github, didit, agent}.
  external        JSONB,
  CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
  CHECK ((status = 'completed') = (completed_at IS NOT NULL))
);

-- At most one open request per person. A second press of the button while one
-- is scheduled is the same request, not a second clock.
CREATE UNIQUE INDEX IF NOT EXISTS account_deletion_requests_one_open
  ON account_deletion_requests (user_id) WHERE status IN ('scheduled', 'held');

CREATE INDEX IF NOT EXISTS account_deletion_requests_due
  ON account_deletion_requests (execute_after) WHERE status IN ('scheduled', 'held');

-- Append-only history of each request: requested, cancelled, held, deferred,
-- each external step, completed. The request row says where things stand; this
-- says how they got there, which is what an audit asks.
CREATE TABLE IF NOT EXISTS account_deletion_events (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  request_id  UUID NOT NULL REFERENCES account_deletion_requests(id) ON DELETE CASCADE,
  at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  kind        TEXT NOT NULL,
  detail      JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS account_deletion_events_request
  ON account_deletion_events (request_id, at);
