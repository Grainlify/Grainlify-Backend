-- When we last asked Didit what this session's status is.
--
-- Drives the reconciler's queue order (least-recently-checked first) rather
-- than recording a decision, so it is stamped on every ATTEMPT, including
-- failures. Stamping only on success would park permanently-failing sessions
-- at the head of the queue forever - six sessions currently return 403 to our
-- API key - and starve every other session behind them.
--
-- NULL means never reconciled, which sorts first: a session we have never
-- checked is the one most likely to have drifted.
ALTER TABLE users ADD COLUMN IF NOT EXISTS kyc_reconciled_at TIMESTAMPTZ;

-- Partial index: the reconciler only ever queues rows that have a session.
CREATE INDEX IF NOT EXISTS idx_users_kyc_reconciled_at
  ON users (kyc_reconciled_at NULLS FIRST)
  WHERE kyc_session_id IS NOT NULL;
