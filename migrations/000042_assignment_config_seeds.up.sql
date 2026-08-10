-- Seed the two config keys the §4 assignment pipeline adds. Kept in the same
-- shape as migration 000039's seed block: every key in
-- internal/hackathon/config.go's Definitions map must have a global-scope
-- (hackathon_id IS NULL) row, and vice versa.
--
--   max_concurrent_applications - AI-specs.md's own §13 open question 1,
--     answered: applications are free, slots are consumed only on winning,
--     so a cap is what stops one account applying to everything.
--   ai_fit_assessment_enabled - the Layer 2 (§4.3) flag. Off by default so
--     the pipeline runs deterministically with no model calls.
INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES
  (NULL, 'max_concurrent_applications', '5'),
  (NULL, 'ai_fit_assessment_enabled', 'false')
ON CONFLICT DO NOTHING;
