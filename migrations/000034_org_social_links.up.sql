CREATE TABLE IF NOT EXISTS org_social_links (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  org_login TEXT NOT NULL,
  telegram TEXT,
  linkedin TEXT,
  whatsapp TEXT,
  twitter TEXT,
  discord TEXT,
  updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_org_social_links_org_login ON org_social_links(LOWER(org_login));
