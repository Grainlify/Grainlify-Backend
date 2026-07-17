-- idempotency_keys table stores cached responses for POST /projects/:id/issues/:number/apply
-- to prevent duplicate applications from client retries, double-clicks, or automated resubmissions.
--
-- The idempotency key is scoped per-user (user_id + idempotency_key is the primary key),
-- so one user cannot retrieve another user's cached response. Keys expire after 24 hours.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    response_status INTEGER NOT NULL,
    response_body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, idempotency_key)
);

-- Index on expires_at for efficient cleanup of expired records.
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_expires_at ON idempotency_keys(expires_at);
