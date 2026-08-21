DROP INDEX IF EXISTS idx_sponsored_claims_one_submission;
DROP INDEX IF EXISTS idx_sponsored_claims_user_recent;
DROP TABLE IF EXISTS sponsored_claims;
ALTER TABLE contributor_addresses DROP COLUMN IF EXISTS public_key;
