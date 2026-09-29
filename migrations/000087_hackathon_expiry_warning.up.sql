-- When the holder of an assignment was warned it was about to lapse.
--
-- On the row rather than in a separate table, because the idempotency comes
-- from it: the warning query sets this in the same statement that selects the
-- rows, so a runner ticking every few minutes for a day warns once instead of
-- hundreds of times.
ALTER TABLE hackathon_assignments ADD COLUMN IF NOT EXISTS expiry_warned_at TIMESTAMPTZ;
