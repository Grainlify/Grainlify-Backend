-- What was said to the contributor, and whether it arrived.
--
-- kyc_reset_audit recorded who reset whom, when, from what status, and a
-- free-text `reason`. Three things were missing, and each has already cost
-- something.
--
-- 1. THE REASON WAS ONE FIELD DOING TWO JOBS.
--
-- `reason` is documented as the admin's justification - the 400 error tells
-- them "say why the contributor is being allowed to verify again" - and it was
-- ALSO concatenated into the notification body and sent to that contributor
-- verbatim. An internal note and a message to the person it is about are not
-- the same text and should never have shared a column. They are split here:
-- reason_code (a fixed, chosen value), note (the admin's words, meant for the
-- contributor), and the existing reason (retained, now purely internal).
--
-- 2. THE PROVIDER'S FINDING IS DESTROYED BY THE NEXT DECISION.
--
-- kyc_data holds the decision this reset is a response to. Both the webhook
-- and the status poll overwrite that column WHOLESALE on the next decision, so
-- the moment the contributor retries, the warnings behind the reset are gone -
-- and Start() overwrites it too. Whatever the reset was based on has to be
-- copied at the moment it is read, or it is not recoverable at all.
-- previous_kyc_data is that copy. It is the answer to "why did we tell them
-- that?" asked six weeks later, and there is nowhere else left to ask.
--
-- 3. "WHETHER IT SUCCEEDED" WAS UNRECORDED.
--
-- Notification delivery is best-effort and silent: a failure logs a warning
-- and returns. So a reset that told the contributor nothing looked exactly
-- like one that told them everything. That is not hypothetical - three resets
-- were applied by hand against production and notified nobody, and the only
-- reason anyone knows is that the operator wrote it into the reason text.
-- Modelled on support_requests, which grew the same columns for the same
-- reason: a delivery you cannot see is a delivery you cannot trust.

ALTER TABLE kyc_reset_audit
  -- One of the codes in kycResetReasons (internal/handlers/kyc_reasons.go).
  -- Nullable because the three rows already here predate it, and because a
  -- future backfill must stay able to write without inventing one. The
  -- handler requires it; the schema does not, exactly as `reason` already
  -- works.
  ADD COLUMN IF NOT EXISTS reason_code TEXT,

  -- The admin's own words, in the message the contributor receives. Distinct
  -- from `reason`: this is written to be read by the person, and it is
  -- optional for every code except 'other', which says nothing on its own.
  ADD COLUMN IF NOT EXISTS note TEXT,

  -- The decision that prompted the reset, copied at read time. See (2).
  ADD COLUMN IF NOT EXISTS previous_kyc_data JSONB,

  -- Set when the in-app notification row was written. NULL means the
  -- contributor was not told - which is a different fact from "no reset
  -- happened" and needs to be visible as such.
  ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ,

  -- Why it did not arrive, when it did not. Bounded by the handler; carries no
  -- provider text.
  ADD COLUMN IF NOT EXISTS notify_error TEXT;

-- Find resets that reached nobody. The partial predicate keeps this to the
-- rows that need attention rather than indexing the whole table - and unlike
-- the support_requests index this replaced, it CAN be false: a delivered row
-- has notified_at set and drops out.
CREATE INDEX IF NOT EXISTS idx_kyc_reset_audit_unnotified
  ON kyc_reset_audit(created_at DESC)
  WHERE notified_at IS NULL;
