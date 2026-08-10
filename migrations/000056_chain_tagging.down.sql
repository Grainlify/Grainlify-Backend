DROP INDEX IF EXISTS idx_hackathon_verdicts_chain;
DROP INDEX IF EXISTS idx_hackathon_issues_chain;
ALTER TABLE hackathon_maintainer_payouts DROP COLUMN IF EXISTS chain_id;
ALTER TABLE hackathon_verdicts DROP COLUMN IF EXISTS chain_id;
ALTER TABLE hackathon_issue_applications DROP COLUMN IF EXISTS chain_id;
ALTER TABLE issue_applications DROP COLUMN IF EXISTS chain_id;
ALTER TABLE hackathon_issues DROP COLUMN IF EXISTS chain_id;
