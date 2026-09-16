-- A dispatch attempt can now be REJECTED, as distinct from unacknowledged.
--
-- rejected        KeeperHub definitively ran nothing (401, 403, 400, 404, 410,
--                 422, 429, 402, or a connection that provably never opened).
--                 Its legs are put back to failed and are resumable.
-- unacknowledged  the outcome is indeterminate (timeout, 5xx, 408, 409, an
--                 accepted request with no execution id). Its legs are unknown.
--
-- A rejected attempt may still carry an execution_id: a 402 creates the
-- execution row and marks it error without starting the run.
--
-- MERGE ORDER. This migration is one of a sequence that must reach production
-- in order and be deployed once, after the last: #549, #550, #551, #552, #553,
-- #554, #555, then the PRs that follow. golang-migrate applies only versions
-- newer than the database's current one, so deploying mid-sequence silently
-- skips any lower-numbered migration merged afterwards - 20260916090100, the
-- Base chain row, would never be applied, and nothing would raise an error.
ALTER TABLE keeperhub_dispatch_attempts DROP CONSTRAINT IF EXISTS keeperhub_dispatch_attempts_state_check;
ALTER TABLE keeperhub_dispatch_attempts
  ADD CONSTRAINT keeperhub_dispatch_attempts_state_check
  CHECK (state IN ('sending', 'sent', 'unacknowledged', 'rejected', 'reconciled', 'mismatch'));
