-- Internal calibration labelling. Not part of the product.
--
-- This exists so PRs can be hand-labelled by people, and the AI judged against
-- those labels later. Everything here is deliberately separate from the product
-- tables: nothing in the product reads these, and nothing here writes to
-- github_pull_requests or hackathon_verdicts.
--
-- The one rule that governs the whole design: a labeller must never see the
-- model's opinion, or any other labeller's, before submitting their own. A
-- label that has seen either measures agreement, not judgement, and the set is
-- void. That is enforced in the query layer and in tests; the schema's job is
-- to make the honest version possible - which is why snapshots are frozen and
-- labels are append-only.
--
-- **A labeller is not a user, and not a role.** Labelling needs someone to run
-- the tool locally; it does not need production admin, which approves payouts,
-- verdicts and role changes. Hanging labelling off users.role would mean
-- granting that authority to get a judgement task done, and conflating the two
-- is what raises the question in the first place. So labellers have their own
-- table, their own ids, and no relationship to users at all.

-- Who may label. Deliberately independent of users and of users.role.
--
-- A row here is created by whoever runs the tool locally, against their own
-- database. It grants nothing in the product: no session, no API access, no
-- role. It exists so a label can be attributed to a person and so two people's
-- labels can be told apart for the agreement calculation.
--
-- No foreign key to users. A labeller need not have a Grainlify account, and
-- someone who has one gains nothing here from it.
CREATE TABLE IF NOT EXISTS calibration_labellers (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  -- Short handle used to select who is labelling when the tool starts.
  handle TEXT NOT NULL UNIQUE CHECK (length(trim(handle)) >= 2),
  display_name TEXT NOT NULL,
  -- Set when someone stops labelling, so their existing labels stay
  -- attributed and interpretable while they drop out of the queue.
  retired_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One draw of PRs to label. Reproducible: the seed and the candidate pool are
-- both recorded, so the same draw can be re-derived later.
CREATE TABLE IF NOT EXISTS calibration_samples (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  -- Human label for the draw, e.g. "set-1".
  name TEXT NOT NULL UNIQUE,
  -- The PRNG seed. With candidate_pr_ids below, this makes the draw
  -- deterministic.
  seed BIGINT NOT NULL,
  -- **The candidate pool as it was at draw time.**
  --
  -- Not a query to re-run. The indexed PR set grows as sync runs, so the same
  -- seed against a later database draws a different sample. Reproducible has to
  -- mean "re-derivable from this recorded list plus the seed", and a second
  -- draw excludes the ids already taken from this same pool.
  candidate_pr_ids UUID[] NOT NULL,
  -- Hash of candidate_pr_ids, so a changed pool is detectable rather than
  -- silently producing a different draw under the same seed.
  candidate_hash TEXT NOT NULL,
  -- How the draw was constrained: strata, caps, target counts. Recorded as
  -- given rather than reconstructed, because the parameters are part of what
  -- makes a number interpretable later.
  strata JSONB NOT NULL,
  -- Free text: what this set is for, and what it cannot measure.
  notes TEXT,
  drawn_by UUID REFERENCES calibration_labellers(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One PR in one sample, with its content frozen at draw time.
CREATE TABLE IF NOT EXISTS calibration_sample_prs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sample_id UUID NOT NULL REFERENCES calibration_samples(id) ON DELETE CASCADE,
  -- The product's pull request id, referenced but NOT foreign-keyed.
  --
  -- The tool reads candidates from the product database and writes everything
  -- else locally, so the row this points at is in a different database. A
  -- foreign key here would force a copy of production pull requests into the
  -- local one purely to satisfy it - and the snapshot below already holds
  -- everything a labeller needs, frozen, so the copy would buy nothing.
  --
  -- The consequence, accepted: nothing stops a sample naming a pull request
  -- that no longer exists upstream. That is fine for a labelling set, which is
  -- about what was true at draw time, not about staying in step with a
  -- database it deliberately does not follow.
  pull_request_id UUID NOT NULL,

  -- The pull request number, copied at draw time.
  --
  -- Stored so the local tool never needs the product database again after the
  -- draw: everything the snapshot fetch and the screen need is here or in the
  -- snapshot beside it. Without it, fetching a diff would mean reopening a
  -- connection to production to translate an id into a number.
  pr_number INT NOT NULL,

  -- Which strata this row was drawn to fill, so the sample's composition is
  -- inspectable without re-deriving it.
  project_full_name TEXT NOT NULL,
  merged BOOLEAN NOT NULL,
  size_band TEXT NOT NULL CHECK (size_band IN ('tiny', 'small', 'medium', 'large', 'huge')),

  -- **Held back at draw time, before any labelling.**
  --
  -- Excluded from every comparison run until released_at is set. The point is
  -- to tell real improvement from fitting to the sample we already looked at,
  -- which only works if the hold-back is chosen before anyone sees the PRs and
  -- is never quietly included.
  held_back BOOLEAN NOT NULL DEFAULT false,
  released_at TIMESTAMPTZ,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (sample_id, pull_request_id)
);

CREATE INDEX IF NOT EXISTS idx_calibration_sample_prs_sample
  ON calibration_sample_prs(sample_id) WHERE NOT held_back;

-- What the labeller sees, frozen at draw time.
--
-- Snapshotted rather than fetched live for two reasons. A force-push or a
-- follow-up commit would otherwise mean two labellers labelled different
-- artefacts under one id, and a comparison run months later would grade the
-- model on a diff nobody labelled. Issue bodies get edited for the same
-- reason, so the linked issue is frozen too.
CREATE TABLE IF NOT EXISTS calibration_pr_snapshots (
  sample_pr_id UUID PRIMARY KEY REFERENCES calibration_sample_prs(id) ON DELETE CASCADE,

  title TEXT NOT NULL,
  body TEXT,
  author_login TEXT,
  url TEXT,

  -- From the GitHub API at draw time; we do not store these in the product.
  additions INT NOT NULL,
  deletions INT NOT NULL,
  changed_files INT NOT NULL,
  diff TEXT NOT NULL,
  -- Truncated when the diff exceeds the cap, so the screen can say so rather
  -- than showing a silently partial diff.
  diff_truncated BOOLEAN NOT NULL DEFAULT false,
  files JSONB NOT NULL,

  -- The linked issue, frozen. NULL where the PR references none: about one PR
  -- in nine links nothing, and the screen must say "no linked issue" rather
  -- than render an empty panel, which reads as "criteria missing" and changes
  -- how someone labels.
  issue_number INT,
  issue_title TEXT,
  issue_body TEXT,

  -- The head commit the diff was taken at, so a later reader can tell whether
  -- the PR moved after labelling.
  head_sha TEXT,
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One labeller's verdict on one PR. Append-only.
--
-- A changed mind writes a new row; the original stays. Enforced by trigger
-- below rather than by convention in Go, because "we always insert" is a habit
-- and habits lapse.
CREATE TABLE IF NOT EXISTS calibration_labels (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sample_pr_id UUID NOT NULL REFERENCES calibration_sample_prs(id) ON DELETE CASCADE,
  -- The labeller, not a user. See calibration_labellers above.
  labeller_id UUID NOT NULL REFERENCES calibration_labellers(id) ON DELETE RESTRICT,

  verdict TEXT NOT NULL CHECK (verdict IN ('accept', 'reject')),
  -- Required, and required to be substantive. A verdict without a reason is
  -- unusable for calibration: when the model disagrees, the reason is the only
  -- thing that says whether the model or the labeller was wrong.
  reason TEXT NOT NULL CHECK (length(trim(reason)) >= 10),
  confidence TEXT NOT NULL CHECK (confidence IN ('certain', 'borderline')),

  -- Supersedes an earlier label by the same person on the same PR. The earlier
  -- row is untouched.
  supersedes_id UUID REFERENCES calibration_labels(id) ON DELETE SET NULL,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_calibration_labels_pr ON calibration_labels(sample_pr_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_calibration_labels_labeller ON calibration_labels(labeller_id, sample_pr_id);

-- Append-only, enforced by the database.
--
-- An UPDATE would rewrite history that a calibration number depends on, and a
-- DELETE would remove the evidence that a label ever changed. Both are refused
-- here so that no code path - including a migration, a psql session, or a
-- future handler written by someone who did not read this file - can do it by
-- accident.
CREATE OR REPLACE FUNCTION calibration_labels_are_append_only()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION
    'calibration_labels is append-only: % refused. A changed label is a new row with supersedes_id set; the original stays.',
    TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_calibration_labels_append_only ON calibration_labels;
CREATE TRIGGER trg_calibration_labels_append_only
  BEFORE UPDATE OR DELETE ON calibration_labels
  FOR EACH ROW EXECUTE FUNCTION calibration_labels_are_append_only();
