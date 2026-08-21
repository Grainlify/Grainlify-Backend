-- Rows carrying the new value must go before the old constraint can hold again.
DELETE FROM settlement_holds WHERE reason = 'kyc_unresolved';
UPDATE settlement_lines SET excluded_reason = NULL WHERE excluded_reason = 'kyc_unresolved';

ALTER TABLE settlement_holds DROP CONSTRAINT IF EXISTS settlement_holds_reason_check;
ALTER TABLE settlement_holds
  ADD CONSTRAINT settlement_holds_reason_check
  CHECK (reason IN ('no_address', 'no_github_account'));

ALTER TABLE settlement_lines DROP CONSTRAINT IF EXISTS founding_settlement_lines_excluded_reason_check;
ALTER TABLE settlement_lines
  ADD CONSTRAINT founding_settlement_lines_excluded_reason_check
  CHECK (excluded_reason IS NULL OR excluded_reason IN ('no_address', 'no_github_account'));
