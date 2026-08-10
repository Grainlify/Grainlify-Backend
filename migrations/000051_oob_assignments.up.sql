-- AI-specs.md §2.3: "Record the event against the maintainer and org" and
-- "Surface it in the admin dashboard."
--
-- One row per distinct out-of-band assignment - a GrainHack issue assigned on
-- GitHub to an account with no matching Grainlify application.
--
-- Recorded whether or not the revert fired. auto_revert_oob_assignment
-- controls whether Grainlify writes to someone else's repository; it does not
-- control whether Grainlify notices. An admin turning the write off is asking
-- us to stop touching their repo, not asking us to stop keeping records - and
-- since this feeds maintainer-pool eligibility (§7), a switch that also
-- erased the evidence would be the single most useful thing for a maintainer
-- gaming the event to turn off.
CREATE TABLE IF NOT EXISTS hackathon_oob_assignments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

  -- Nullable: EffectiveGrainHackLabels widens enforcement to any project with
  -- a literal "GrainHack" label, including ones outside a hackathon, so these
  -- can be detected with no hackathon to attribute them to.
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE SET NULL,
  project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

  -- The grain the threshold counts on. §2.3 flags the *org*, and §3.3's
  -- oob_assignment_flag_threshold is described as "repeated out-of-band
  -- assignments from an org". Denormalised at insert for the same reason
  -- hackathon_issues.org_login is.
  org_login TEXT NOT NULL,
  issue_number INT NOT NULL,

  -- The account that was assigned. This is the actionable part: three
  -- assignments to three different newcomers reads very differently from
  -- three to the same unfamiliar account.
  assigned_login TEXT NOT NULL,

  -- The maintainer held responsible: the project owner on Grainlify.
  --
  -- NOT the actor who performed the assignment. GitHub reports that only on
  -- the issues.assigned webhook's sender, and this reconciliation runs from a
  -- full issue listing in the sync worker - sync_jobs carries just
  -- (project_id, job_type, status, run_at), so the sender never reaches here.
  -- Capturing the true actor would mean threading it through sync_jobs.
  -- Recorded as responsibility, not attribution, and the admin view says so.
  maintainer_user_id UUID REFERENCES users(id) ON DELETE SET NULL,

  -- Whether Grainlify removed the assignee and commented, which is exactly
  -- what auto_revert_oob_assignment decides. Stored per event because the
  -- setting can change between events, and "why was this one left alone?" is
  -- a question an admin will ask.
  reverted BOOLEAN NOT NULL DEFAULT false,
  reverted_at TIMESTAMPTZ,

  -- Re-observation counter. The sync worker re-reads every issue on every
  -- run, so a standing out-of-band assignment that was NOT reverted would
  -- otherwise be recorded again on every tick and inflate the count until it
  -- crossed the threshold on its own. See RecordOOBAssignment for when this
  -- is incremented and when it is only a last_seen_at bump.
  occurrences INT NOT NULL DEFAULT 1,
  first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  UNIQUE (project_id, issue_number, assigned_login)
);

CREATE INDEX IF NOT EXISTS idx_hackathon_oob_org
  ON hackathon_oob_assignments(org_login, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_hackathon_oob_hackathon
  ON hackathon_oob_assignments(hackathon_id);
CREATE INDEX IF NOT EXISTS idx_hackathon_oob_maintainer
  ON hackathon_oob_assignments(maintainer_user_id);
