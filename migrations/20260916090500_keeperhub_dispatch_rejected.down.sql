-- Fails if any attempt is already recorded as rejected, deliberately: rewriting
-- those rows would change what the audit trail says happened.
ALTER TABLE keeperhub_dispatch_attempts DROP CONSTRAINT IF EXISTS keeperhub_dispatch_attempts_state_check;
ALTER TABLE keeperhub_dispatch_attempts
  ADD CONSTRAINT keeperhub_dispatch_attempts_state_check
  CHECK (state IN ('sending', 'sent', 'unacknowledged', 'reconciled', 'mismatch'));
