-- "Delivered" is category-dependent, and the previous index did not know that.
--
-- KYC requests go to the admin's DM ONLY - no public post. The group is
-- readable without joining, and a stub reading "🪪 KYC request #1234 - replied
-- privately" discloses that somebody asked a verification question at a
-- timestamp, which correlates against anything else in the group at that
-- moment and identifies the person the redaction existed to protect. It told
-- nobody anything actionable: the reporter was already acknowledged in the
-- widget, the admin gets the DM, and a bystander cannot act on it.
--
-- So each category has exactly one Telegram delivery, and they are different
-- columns:
--
--   kyc        telegram_admin_dm_delivered_at   telegram_delivered_at stays NULL
--   everything else  telegram_delivered_at      telegram_admin_dm_delivered_at stays NULL
--
-- idx_support_requests_undelivered was written before that split and reads
--
--   discord_delivered_at IS NULL OR telegram_delivered_at IS NULL
--     OR telegram_admin_dm_delivered_at IS NULL
--
-- under which EVERY row matches forever, because each category permanently
-- leaves one of the two NULL by design. Harmless for an index, fatal for
-- anything built on it: a replay job would redeliver every request, every run,
-- for ever. That is a predicate which can never be false - the same shape as a
-- check that can never fail, inverted.
--
-- The replacement encodes the real rule. It is kept byte-identical to
-- supportUndeliveredPredicate in internal/handlers/support_delivery.go, and
-- TestSupportDelivered_SQLAndGoAgree asserts the SQL and the Go function
-- return the same answer for every combination rather than trusting that two
-- copies stay in step.
DROP INDEX IF EXISTS idx_support_requests_undelivered;

CREATE INDEX IF NOT EXISTS idx_support_requests_undelivered
  ON support_requests(created_at DESC)
  WHERE discord_delivered_at IS NULL
     OR (category = 'kyc' AND telegram_admin_dm_delivered_at IS NULL)
     OR (category <> 'kyc' AND telegram_delivered_at IS NULL);

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
