-- Judging-pipeline config the §5 stages consume. ai_judging_enabled gates
-- stages 3-5 (the model calls); stages 1, 2 and 6 run regardless.
--
-- judging_shadow_mode is on by default: AI-specs.md §12's rollout says
-- event 1 should run judging but publish nothing and pay by hand. Making
-- that the default means shadow mode is what you get unless someone
-- deliberately turns it off.
INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES
  (NULL, 'ai_judging_enabled', 'false'),
  (NULL, 'judging_shadow_mode', 'true')
ON CONFLICT DO NOTHING;
