-- The evidence that the mandatory pre-dispatch simulation actually ran.
--
-- KeeperHub's dry-run (execute_transfer with simulate:true) is the single most
-- on-brand part of this integration and, until now, had no caller anywhere in
-- the codebase - no route, no admin screen, no CLI. Release now simulates
-- every leg it is about to send before claiming or dispatching any of them,
-- and refuses the whole release if any leg would revert, reports an error, or
-- cannot be simulated at all (an unreachable simulator fails exactly as hard
-- as a leg that would revert - see internal/keeperhubrail/preflight.go).
--
-- These columns are the record that the check ran for a leg that WAS
-- dispatched, not only that the feature exists. A leg that would have
-- reverted never reaches this table at all - the whole release is refused
-- before any leg is claimed - so preflight_would_revert is always false here
-- in practice; it is stored anyway because this is a record of what was
-- checked, matching TransferSimulation.Raw's own reasoning for keeping the
-- whole response rather than only a boolean.
--
-- NOT NULL with a default rather than nullable: no release has ever
-- dispatched a leg in production (the release endpoint itself only just
-- landed on main), so there is no pre-existing row this default has to paper
-- over - it exists so a future ALTER TABLE ADD COLUMN in this family stays
-- this cheap, not because real data needs it.
ALTER TABLE keeperhub_dispatch_attempt_legs
  ADD COLUMN IF NOT EXISTS preflight_would_revert BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE keeperhub_dispatch_attempt_legs
  ADD COLUMN IF NOT EXISTS preflight_checked_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- The whole simulation response, kept for the same reason
-- TransferSimulation.Raw is kept: the fields KeeperHub returns vary by action,
-- and a field dropped here is one nobody can get back later. Nullable: a leg
-- whose simulation errored outright (preflight unavailable, not a revert)
-- never produced a body to keep, and the release that dispatched it never
-- happened either way.
ALTER TABLE keeperhub_dispatch_attempt_legs
  ADD COLUMN IF NOT EXISTS preflight_raw JSONB;
