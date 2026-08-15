-- "Delivered" is category-dependent, and the previous index did not know that.
--
-- A KYC request reaches exactly one destination: the admin's Telegram DM. It
-- is kept out of both of the others deliberately.
--
--   * Not posted to a Telegram topic. @Grainlify is readable without joining,
--     and a stub reading "KYC request #1234 - replied privately" discloses
--     that somebody asked a verification question at a timestamp, which
--     correlates against anything else in the group at that moment and
--     identifies the person the redaction existed to protect. It told nobody
--     anything actionable either: the reporter was already acknowledged in the
--     widget, and the admin gets the DM.
--
--   * Not sent to Discord either. The Discord embed carries the message, the
--     page, any screenshot AND the reporter's GitHub login, so routing KYC
--     away from a public Telegram topic while still posting it to Discord
--     would have achieved nothing. Keeping the Discord channel private instead
--     would have made the privacy of every verification request a property of
--     a channel permission - configuration that anyone with Manage Channels
--     can change at any time without it being noticed. It is held in the sink
--     instead, where a test can prove it.
--
-- So the columns each category actually uses differ:
--
--   kyc              telegram_admin_dm_delivered_at
--                    discord_delivered_at and telegram_delivered_at stay NULL
--   everything else  discord_delivered_at AND telegram_delivered_at
--                    telegram_admin_dm_delivered_at stays NULL
--
-- idx_support_requests_undelivered was written before any of that and reads
--
--   discord_delivered_at IS NULL OR telegram_delivered_at IS NULL
--     OR telegram_admin_dm_delivered_at IS NULL
--
-- under which EVERY row matches forever, because each category permanently
-- leaves at least one of the three NULL by design. Harmless for an index,
-- fatal for anything built on it: a replay job would redeliver every request,
-- every run, for ever. That is a predicate which can never be false - the same
-- shape as a check that can never fail, inverted.
--
-- The replacement encodes the real rule, one branch per category, because the
-- categories no longer share a shape: KYC waits on one column, everything else
-- waits on two. It is kept byte-identical to supportUndeliveredPredicate in
-- internal/handlers/support_delivery.go - asserted by
-- TestUndeliveredIndexMatchesTheGoPredicate, which reads THIS FILE, and by
-- TestSupportDelivered_SQLAndGoAgree, which evaluates the predicate in
-- Postgres against the Go function for every combination of column states.
-- Two copies of one rule is how the leaderboard and the profile came to
-- disagree about the same contributor.
DROP INDEX IF EXISTS idx_support_requests_undelivered;

CREATE INDEX IF NOT EXISTS idx_support_requests_undelivered
  ON support_requests(created_at DESC)
  WHERE (category = 'kyc' AND telegram_admin_dm_delivered_at IS NULL)
     OR (category <> 'kyc' AND (discord_delivered_at IS NULL OR telegram_delivered_at IS NULL));

-- Whether a topic post had to fall back to the group's General thread.
--
-- If a topic is deleted, or the bot loses can_manage_topics, sendMessage with
-- that message_thread_id fails. The submission must not break, so the sink
-- retries without the thread id and the message lands in General. That IS a
-- delivery - the report reached a human - but a misrouted one, and without
-- this flag it is indistinguishable from a correctly routed one. Reports
-- quietly piling into General while a topic sits empty is exactly the kind of
-- thing nobody notices for a week.
ALTER TABLE support_requests
  ADD COLUMN IF NOT EXISTS telegram_routed_to_fallback BOOLEAN NOT NULL DEFAULT false;
