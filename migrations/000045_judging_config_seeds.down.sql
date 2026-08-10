DELETE FROM hackathon_config_settings
WHERE hackathon_id IS NULL AND key IN ('ai_judging_enabled', 'judging_shadow_mode');
