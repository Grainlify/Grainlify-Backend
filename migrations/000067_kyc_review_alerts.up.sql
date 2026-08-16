-- One row per Didit session we have alerted an admin about.
--
-- A session flagged "In Review" is routed to OUR manual-review queue, not
-- Didit's - their docs are explicit: "status='In Review' - route to your
-- manual-review queue". Nothing told anybody that queue existed, so it was
-- discoverable only by a contributor complaining, which is how three sessions
-- came to sit there for up to 22 hours. Every hour costs the contributor: the
-- founding-pool wave is allocated first-come at the moment verification
-- completes, and the multiplier that comes with it is permanent.
--
-- Keyed on the SESSION, not the user. A user can have several sessions over
-- time and each one genuinely needs its own review; keying on user_id would
-- silence the second.
--
-- The primary key is what makes "alert once" true rather than intended. Both
-- the webhook and the sweep insert with ON CONFLICT DO NOTHING and send only
-- when they actually inserted, so a webhook and a sweep racing on the same
-- session produce one message, not two. An alert that repeats gets muted, and
-- a muted alert is the queue we already had.
CREATE TABLE IF NOT EXISTS kyc_review_alerts (
  session_id TEXT PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 'webhook' or 'sweep'. Recorded so it is visible whether Didit's delivery
  -- is doing the work or the fallback is carrying it - Didit retries twice
  -- and then drops, so a five-minute outage on our side loses the event
  -- permanently and the sweep is the only thing that would notice.
  source TEXT NOT NULL CHECK (source IN ('webhook', 'sweep')),
  alerted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_kyc_review_alerts_user ON kyc_review_alerts(user_id);
