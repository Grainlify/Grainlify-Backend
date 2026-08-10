-- On-chain spec §6, stage 2: the issue carries the chain.
--
-- Nullable for now. The spec makes chain_id required on hackathon_issues, but
-- every existing row predates chains and there is no correct value to invent
-- for them - a NOT NULL with a default would silently assign real issues to a
-- chain nobody chose. Intake enforces presence for issues entering an event
-- that has chain pools; the constraint tightens once no legacy rows remain.
ALTER TABLE hackathon_issues ADD COLUMN IF NOT EXISTS chain_id TEXT;

-- Copied from the issue at application time and never updated (§6).
-- Denormalised deliberately: it is the locked record of which pool this
-- application belongs to, and it must survive the issue being re-tagged.
ALTER TABLE issue_applications ADD COLUMN IF NOT EXISTS chain_id TEXT;
ALTER TABLE hackathon_issue_applications ADD COLUMN IF NOT EXISTS chain_id TEXT;

-- Payouts are per chain, so a verdict has to say which pool paid it.
ALTER TABLE hackathon_verdicts ADD COLUMN IF NOT EXISTS chain_id TEXT;
ALTER TABLE hackathon_maintainer_payouts ADD COLUMN IF NOT EXISTS chain_id TEXT;

CREATE INDEX IF NOT EXISTS idx_hackathon_issues_chain ON hackathon_issues(hackathon_id, chain_id);
CREATE INDEX IF NOT EXISTS idx_hackathon_verdicts_chain ON hackathon_verdicts(hackathon_id, chain_id);
