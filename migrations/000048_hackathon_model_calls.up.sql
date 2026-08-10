-- Every model call the judging and assignment pipelines make, kept
-- permanently and verbatim.
--
-- AI-specs.md §6 gives contributors an appeal window, and §5.7 makes human
-- overrides the calibration set. Both need to know exactly what the model
-- was asked and exactly what it said, weeks later. None of that can be
-- reconstructed after the fact: prompts get edited, models get deprecated,
-- and config changes between events. If it isn't written down at call time
-- it is gone.
CREATE TABLE IF NOT EXISTS hackathon_model_calls (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE CASCADE,
  -- The verdict or application this call was made for. Nullable so a call
  -- is still logged even if the row it was for is later deleted - the log
  -- is the record of what happened, not a child of the thing it judged.
  verdict_id UUID REFERENCES hackathon_verdicts(id) ON DELETE SET NULL,
  application_id UUID REFERENCES hackathon_issue_applications(id) ON DELETE SET NULL,
  -- 'judge' | 'cross_check' | 'escalation' | 'fit_assessment'
  stage TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  prompt_version TEXT,
  -- The §1.1 snapshot the event was running under, so a verdict can be
  -- reproduced against the rules actually in force at the time.
  config_snapshot_taken_at TIMESTAMPTZ,
  request JSONB NOT NULL,
  response JSONB,
  -- Set when the call failed or returned something that didn't match the
  -- tool schema. A malformed response is kept verbatim rather than
  -- discarded: it is the evidence for why a verdict went to a human.
  error TEXT,
  duration_ms INT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_hackathon_model_calls_verdict ON hackathon_model_calls(verdict_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_hackathon_model_calls_hackathon ON hackathon_model_calls(hackathon_id, stage, created_at DESC);
