DROP TABLE IF EXISTS payout_event_roots;
DROP TABLE IF EXISTS chain_reconcile_alerts;
DELETE FROM support_requests WHERE category = 'payout';
ALTER TABLE support_requests DROP CONSTRAINT IF EXISTS support_requests_category_check;
ALTER TABLE support_requests
  ADD CONSTRAINT support_requests_category_check
  CHECK (category IN ('bug', 'kyc', 'idea', 'help', 'other'));
