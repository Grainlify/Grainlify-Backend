-- Founding Contributor Pool: replaces the fixed-rate points programme.
--
-- The points system paid a known amount for an action that costs nothing and
-- produces nothing. This pays shares out of one fixed, announced pool, and a
-- share's value cannot be known by anyone until settlement - the same
-- property GrainHack's unit_value has, for the same reason: a reward you can
-- calculate in advance is a reward you can farm.

-- Wave membership. Assigned once, at verification, and never recomputed.
--
-- sequence_number is the join order across every wave, gapless, and is what
-- decides the wave. Storing the wave alongside it is denormalised on purpose:
-- the wave must survive a boundary change, and recomputing it from the
-- sequence would silently move people between waves the moment a boundary
-- moved. That is the failure this design exists to prevent.
CREATE TABLE IF NOT EXISTS founding_members (
  user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  sequence_number INT NOT NULL UNIQUE,
  wave TEXT NOT NULL CHECK (wave IN ('founding', 'wave_two', 'open')),
  -- Captured at assignment so a later config edit cannot revalue shares
  -- somebody already earned.
  multiplier NUMERIC(6,3) NOT NULL CHECK (multiplier > 0),
  assigned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_founding_members_wave ON founding_members(wave);

-- The boundaries in force, captured the first time anyone is assigned.
--
-- Wave sizes are config, but §4.1: a wave that silently widens after launch
-- tells everyone that Grainlify's announced limits are not real, and this
-- platform's whole anti-farming design rests on published rules being
-- credible. So the boundaries are frozen at first assignment and every later
-- assignment reads THIS row, not config. A single row, enforced by the
-- primary key on a constant.
CREATE TABLE IF NOT EXISTS founding_wave_lock (
  id BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
  founding_slots INT NOT NULL CHECK (founding_slots > 0),
  wave_two_slots INT NOT NULL CHECK (wave_two_slots > 0),
  multiplier_founding NUMERIC(6,3) NOT NULL CHECK (multiplier_founding > 0),
  multiplier_wave_two NUMERIC(6,3) NOT NULL CHECK (multiplier_wave_two > 0),
  multiplier_open NUMERIC(6,3) NOT NULL CHECK (multiplier_open > 0),
  locked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only ledger, one row per share-earning event.
--
-- Raw shares only - the wave multiplier is NOT applied here. It multiplies a
-- member's whole total at settlement (§2.2), so applying it per row would
-- bake a factor into history that a correction could no longer undo, and
-- would make the ledger disagree with the arithmetic it is supposed to
-- explain.
--
-- source_ref is the id of the row that caused the entry (a referral, a
-- verdict) so every share is traceable back to the thing that earned it -
-- the same reconstructability requirement every other artefact here has.
CREATE TABLE IF NOT EXISTS founding_shares (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  shares NUMERIC(12,4) NOT NULL CHECK (shares > 0),
  reason TEXT NOT NULL CHECK (reason IN (
    'verified_account',
    'merged_pr',
    'referral_verified',
    'referral_merged_pr'
  )),
  source_ref UUID,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_founding_shares_user ON founding_shares(user_id);

-- One share-earning event may only ever be counted once. Without this a
-- retried webhook or a re-run backfill silently doubles somebody's shares,
-- and the ledger cannot tell the difference between a duplicate and a genuine
-- second event.
CREATE UNIQUE INDEX IF NOT EXISTS idx_founding_shares_unique_event
  ON founding_shares(user_id, reason, COALESCE(source_ref, '00000000-0000-0000-0000-000000000000'::uuid));

-- The computed settlement. Written once, released never (so far): §7 of the
-- redesign - there is no disbursement path, and computing a number is not the
-- same as owing it.
--
-- usdc_amount is stored and deliberately not exposed: §6 forbids publishing
-- any per-person figure, and a settlement result reaching a profile page
-- would publish exactly that.
CREATE TABLE IF NOT EXISTS founding_settlements (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE SET NULL,
  pool_usdc NUMERIC(18,6) NOT NULL,
  total_shares NUMERIC(16,4) NOT NULL CHECK (total_shares >= 0),
  share_value_usdc NUMERIC(18,8) NOT NULL,
  computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Set only if a release path is ever built and used.
  released_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS founding_settlement_lines (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  settlement_id UUID NOT NULL REFERENCES founding_settlements(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  raw_shares NUMERIC(12,4) NOT NULL,
  multiplier NUMERIC(6,3) NOT NULL,
  effective_shares NUMERIC(12,4) NOT NULL,
  usdc_amount NUMERIC(18,6) NOT NULL,
  -- Why somebody got nothing is as important as why somebody got something.
  ineligible_reason TEXT,
  UNIQUE (settlement_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_founding_settlement_lines_settlement
  ON founding_settlement_lines(settlement_id);
