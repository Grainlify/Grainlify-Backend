-- Shadow model runs for the calibration set.
--
-- A run scores one model against the labels already recorded. It never writes
-- to calibration_labels: the human judgement is the fixed point everything
-- else is measured against, and a comparison that could edit it would not be
-- a comparison.

CREATE TABLE IF NOT EXISTS calibration_model_runs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  sample_id UUID NOT NULL REFERENCES calibration_samples(id) ON DELETE CASCADE,

  provider TEXT NOT NULL,
  model TEXT NOT NULL,

  -- The prompt, identified two ways.
  --
  -- prompt_version is the human name. prompt_sha256 is the hash of the actual
  -- system prompt text used, so a change is detectable from the run log rather
  -- than only from a diff nobody reads. Two runs that disagree while claiming
  -- the same version are then a question the data can answer.
  prompt_version TEXT NOT NULL,
  prompt_sha256 TEXT NOT NULL,

  -- The input-token count above which this provider's long-context pricing
  -- applies. Recorded per run rather than assumed at read time, because it is
  -- a pricing fact that changes and a stored number can be checked.
  long_context_threshold INT NOT NULL,

  -- Per-million-token prices in effect for this run, so a cost figure can be
  -- recomputed and audited later.
  input_price_per_mtok NUMERIC(10,4) NOT NULL,
  output_price_per_mtok NUMERIC(10,4) NOT NULL,

  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  notes TEXT
);

CREATE INDEX IF NOT EXISTS idx_calibration_model_runs_sample ON calibration_model_runs(sample_id, started_at DESC);

-- One model's verdict on one pull request.
CREATE TABLE IF NOT EXISTS calibration_model_verdicts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id UUID NOT NULL REFERENCES calibration_model_runs(id) ON DELETE CASCADE,
  sample_pr_id UUID NOT NULL REFERENCES calibration_sample_prs(id) ON DELETE CASCADE,

  -- NULL when the call failed. A failed call is recorded rather than dropped:
  -- a run of 18 successes out of 20 must not be reported as 18 of 18.
  verdict TEXT CHECK (verdict IS NULL OR verdict IN ('accept', 'reject')),
  reason TEXT,
  error TEXT,

  prompt_tokens INT,
  completion_tokens INT,
  -- Whether this call's input crossed the run's long-context threshold. Stored
  -- rather than derived so the flag survives a later change to the threshold.
  long_context BOOLEAN NOT NULL DEFAULT false,
  duration_ms INT,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (run_id, sample_pr_id)
);

CREATE INDEX IF NOT EXISTS idx_calibration_model_verdicts_run ON calibration_model_verdicts(run_id);
