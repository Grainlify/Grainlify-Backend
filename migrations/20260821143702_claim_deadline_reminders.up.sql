-- Which reminders we have already sent, keyed so an extension re-opens them.
--
-- # Why deadline_unix is in the key
--
-- The obvious key is (settlement, user, milestone), and it is wrong. The
-- deadline is read live from chain and `extend_deadline` is the documented
-- remedy for a late claimant, so a window can move at any time. When it does,
-- T-14/T-7/T-3 recompute against the NEW date and SHOULD fire again - it is a
-- new window, and somebody warned about the old date needs warning about the
-- new one.
--
-- A milestone-only key suppresses exactly that. Including the deadline gives an
-- extension a fresh key space for free, and it falls straight out of the rule
-- that the deadline is never cached.
--
-- # What this cannot do, said here rather than discovered
--
-- The reminders are IN-APP ONLY. notifications.NotifyInApp sends no email, and
-- users.email is non-null for zero accounts - so these reach somebody who
-- visits the site and nobody who does not. The people this exists for are the
-- ones slowest to set up a wallet, who are by definition the least likely to be
-- looking. See #521: a merged reminder feature is not evidence that anybody was
-- told.
CREATE TABLE IF NOT EXISTS claim_deadline_reminders (
  settlement_id UUID NOT NULL,
  user_id       UUID NOT NULL REFERENCES users(id),

  -- 't_minus_14' | 't_minus_7' | 't_minus_3' | 'passed'
  milestone     TEXT NOT NULL CHECK (milestone IN ('t_minus_14', 't_minus_7', 't_minus_3', 'passed')),

  -- The deadline this reminder was computed against, so an extension is a
  -- different row rather than a suppressed one.
  deadline_unix BIGINT NOT NULL,

  sent_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (settlement_id, user_id, milestone, deadline_unix)
);

CREATE INDEX IF NOT EXISTS idx_claim_deadline_reminders_user
  ON claim_deadline_reminders (user_id);
