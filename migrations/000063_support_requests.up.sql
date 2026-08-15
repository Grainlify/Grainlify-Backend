-- Support requests get written down before anybody tries to deliver them.
--
-- Until now they were not written down at all. POST /bug-reports relayed
-- straight to a Discord webhook and bug_reports.go said so plainly: "Reports
-- are not persisted anywhere in this database - the Discord channel is the
-- system of record." So a failed webhook call returned 502 and the report was
-- gone, along with whatever the person had typed. The one place a support
-- system must not lose data is the moment somebody tells you something is
-- broken.
--
-- This table is the record. Delivery to Discord (and later Telegram) becomes a
-- best-effort fan-out afterwards, with per-sink timestamps here so a sink that
-- was down can be identified and replayed rather than guessed at.
CREATE TABLE IF NOT EXISTS support_requests (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Nullable: the widget is open to anonymous visitors, and somebody who
  -- cannot sign in is exactly the person most likely to need support.
  --
  -- Set from the JWT, never from the request body. The endpoint previously
  -- accepted a `reporter_login` field from the client and relayed it as the
  -- reporter's identity, which meant anyone could file a report as anyone.
  -- Harmless-looking while the sink was a private Discord; an impersonation
  -- vector the moment a sink is a public group.
  user_id UUID REFERENCES users(id) ON DELETE SET NULL,

  -- Which widget category this came from. Constrained rather than free text so
  -- a typo cannot silently create a sixth bucket that routes nowhere.
  -- 'bug' is the default because it is the only category the widget offers
  -- today; the rest land with the category picker.
  category TEXT NOT NULL DEFAULT 'bug'
    CHECK (category IN ('bug', 'kyc', 'idea', 'help', 'other')),

  message TEXT NOT NULL,
  page_url TEXT,
  user_agent TEXT,

  -- Named _url for the shape this will take once there is an object store.
  -- Today it holds the base64 data: URL as submitted, which is the same
  -- convention social_follow_submissions already uses for screenshots. Stored
  -- rather than dropped because a bug report without its screenshot is often
  -- not actionable, and the alternative is losing it at the same moment the
  -- delivery fails.
  screenshot_url TEXT,

  -- Recorded for abuse investigation, and deliberately NOT sent to any sink.
  -- The Discord payload has been including the reporter's IP; a public
  -- Telegram topic must never see it, and neither should Discord by default.
  reporter_ip TEXT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Per-sink delivery, so "did this reach anyone?" is answerable per sink
  -- rather than as one boolean. NULL means not delivered - either not yet
  -- attempted, or attempted and failed. The distinction that matters
  -- operationally is delivered vs not; why it failed goes to the log.
  discord_delivered_at TIMESTAMPTZ,
  telegram_delivered_at TIMESTAMPTZ,

  -- The admin direct message, tracked SEPARATELY from the topic post above.
  --
  -- KYC requests are two deliveries, not one. The public topic gets a stub
  -- ("🪪 KYC request #1234 — replied privately") because the group is readable
  -- by anyone and KYC questions name individuals and mention refusals; the
  -- details go to the admin's DM. Those can fail independently.
  --
  -- Sharing one telegram_delivered_at would create the worst available state:
  -- the topic post succeeds, the DM 403s, and the public group carries a
  -- claim that somebody was replied to privately while the details reached
  -- nobody - a request that looks handled and has actually vanished. One
  -- column could not tell that apart from a clean delivery.
  --
  -- For a KYC request this is the column that means "the information arrived".
  -- telegram_delivered_at only means the stub was posted.
  telegram_admin_dm_delivered_at TIMESTAMPTZ
);

-- The admin read is "what came in recently", and the replay read is "what has
-- not reached a sink".
CREATE INDEX IF NOT EXISTS idx_support_requests_created
  ON support_requests(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_support_requests_undelivered
  ON support_requests(created_at DESC)
  WHERE discord_delivered_at IS NULL
     OR telegram_delivered_at IS NULL
     OR telegram_admin_dm_delivered_at IS NULL;

-- The query that matters most operationally: a KYC request whose stub was
-- posted publicly but whose details never reached the admin. That is the
-- silent-failure shape - it looks handled from the group and is invisible
-- everywhere else.
CREATE INDEX IF NOT EXISTS idx_support_requests_kyc_dm_missing
  ON support_requests(created_at DESC)
  WHERE category = 'kyc' AND telegram_admin_dm_delivered_at IS NULL;
