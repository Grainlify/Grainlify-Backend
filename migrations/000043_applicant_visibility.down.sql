DELETE FROM hackathon_config_settings
WHERE hackathon_id IS NULL AND key = 'applicant_count_visibility';
