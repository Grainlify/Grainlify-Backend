-- What the bounty agent tells the backend about GrainHack payouts, and which
-- in-app notices those facts have already produced.
--
-- # grainhack_payment_reports
--
-- One row per winner per event pool, ever: the signer pays a winner once, so a
-- second confirmed payment for the same person is not a duplicate report, it is
-- an alarm, and the UNIQUE refuses it. A transaction signature belongs to one
-- payment. The backend does not read the chain; the row records what the agent
-- reported, authenticated by GRAINHACK_STATEMENT_TOKEN, and checked against the
-- signed statement it names (line exists, payable, same amount, same network).
--
-- # grainhack_notices
--
-- Each (event pool, winner, kind) notice is sent once. The agent may report a
-- missing wallet on every import and a superseding statement re-lists held
-- winners; without this the same person would be told the same thing again
-- each time.
--
-- # grainhack_broadcast_notices
--
-- One-time notices to a group (the Base Sepolia address owners when GrainHack
-- moved to Solana). The key makes a second send refuse instead of repeating.

CREATE TABLE IF NOT EXISTS grainhack_payment_reports (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID NOT NULL REFERENCES hackathons(id),
  pool TEXT NOT NULL CHECK (pool IN ('contributor', 'maintainer')),
  github_user_id BIGINT NOT NULL CHECK (github_user_id > 0),
  user_id UUID NOT NULL REFERENCES users(id),
  statement_id UUID NOT NULL REFERENCES grainhack_results_statements(id),
  amount_minor NUMERIC(78, 0) NOT NULL CHECK (amount_minor > 0),
  currency TEXT NOT NULL,
  network TEXT NOT NULL CHECK (network IN ('solana-devnet', 'solana-mainnet')),
  tx_signature TEXT NOT NULL UNIQUE,
  recipient TEXT NOT NULL,
  reported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hackathon_id, pool, github_user_id)
);

CREATE TABLE IF NOT EXISTS grainhack_notices (
  hackathon_id UUID NOT NULL REFERENCES hackathons(id),
  pool TEXT NOT NULL,
  github_user_id BIGINT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('held_kyc', 'link_wallet', 'paid')),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  statement_id UUID NOT NULL REFERENCES grainhack_results_statements(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (hackathon_id, pool, github_user_id, kind)
);

CREATE TABLE IF NOT EXISTS grainhack_broadcast_notices (
  key TEXT PRIMARY KEY,
  sent_by UUID NOT NULL REFERENCES users(id),
  recipients INT NOT NULL CHECK (recipients >= 0),
  sent_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
