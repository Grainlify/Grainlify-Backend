-- Controls how much of an issue's applicant pool a contributor can see
-- while the application window is still open.
--
-- Not in AI-specs.md. Added because an exact live count creates an
-- information asymmetry across the window: someone applying at hour 1 sees
-- nothing, someone at hour 23 sees the full pool and picks the least
-- contested issue. That rewards waiting, concentrates applications at
-- window close, and makes a 24h window functionally much shorter.
--
-- 'bucketed' keeps the useful signal (don't pile onto a crowded issue)
-- without handing late applicants a precision advantage.
INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES
  (NULL, 'applicant_count_visibility', 'bucketed')
ON CONFLICT DO NOTHING;
