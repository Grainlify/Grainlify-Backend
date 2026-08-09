-- Org ratings: once a user has had a PR merged into any repo under a
-- GitHub org, they may leave a 1-5 star rating plus an optional written
-- comment for that org. "Org" has no first-class table anywhere in this
-- schema - it's always SPLIT_PART(projects.github_full_name, '/', 1), so
-- org_login here is a plain string, not a foreign key. One mutable row per
-- (user, org), editable like a product review - not an append-only ledger.
CREATE TABLE IF NOT EXISTS org_ratings (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  org_login TEXT NOT NULL,
  rating SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
  comment TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness (org_login casing should be stable in
-- practice since it's always sourced from the same GitHub account, but
-- matched case-insensitively everywhere else this session's work touches
-- GitHub logins, so kept consistent here too).
CREATE UNIQUE INDEX IF NOT EXISTS idx_org_ratings_user_org ON org_ratings(user_id, LOWER(org_login));
CREATE INDEX IF NOT EXISTS idx_org_ratings_org_login ON org_ratings(LOWER(org_login));
