-- Only the global-scope seeds; per-hackathon overrides an admin set for
-- these keys are theirs, not this migration's to delete.
DELETE FROM hackathon_config_settings
WHERE hackathon_id IS NULL
  AND key IN ('max_concurrent_applications', 'ai_fit_assessment_enabled');
