-- Config values from AI-specs.md §3 - every threshold/weight/cap/window/
-- label is admin-configurable, never hardcoded. Metadata (type, description,
-- valid range, default, section) lives in a static Go map
-- (internal/hackathon/config.go), not here - this table stores only current
-- values, so editing a description never needs a migration. A row with
-- hackathon_id NULL is the global default; a row with hackathon_id set is a
-- per-hackathon override, resolved override > global > factory default.
--
-- Only the issue-intake keys (grainhack_label..auto_revert_oob_assignment)
-- plus merge_grace_period_hours have real consuming logic in this slice
-- (Slice 1 - Foundation). Every other key below is inert - stored and shown
-- in the settings UI, not yet read by any pipeline - so later slices
-- (assignment, judging, payout) don't need another migration just to add a
-- setting, and admins can see/publish the full rule set early.
CREATE TABLE IF NOT EXISTS hackathon_config_settings (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  hackathon_id UUID REFERENCES hackathons(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_hcs_global_key ON hackathon_config_settings(key) WHERE hackathon_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_hcs_hackathon_key ON hackathon_config_settings(hackathon_id, key) WHERE hackathon_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_hcs_hackathon ON hackathon_config_settings(hackathon_id) WHERE hackathon_id IS NOT NULL;

INSERT INTO hackathon_config_settings (key, value) VALUES
  -- §3.2 hackathon setup (only the one non-structural field; name/dates/pools live on hackathons itself)
  ('merge_grace_period_hours', '48'),
  -- §3.3 issue intake - active in Slice 1
  ('grainhack_label', 'grainhack'),
  ('max_issues_per_org', '50'),
  ('require_acceptance_criteria', 'true'),
  ('require_difficulty_tier', 'true'),
  ('allow_late_issue_entry', 'true'),
  ('late_entry_cutoff_hours', '48'),
  ('auto_revert_oob_assignment', 'true'),
  ('oob_assignment_flag_threshold', '3'),
  -- §3.4 contributor slots and caps - inert until the assignment slice
  ('slots_per_contributor', '2'),
  ('slot_freed_on', 'pr_submission'),
  ('max_issues_per_contributor_per_org', '4'),
  ('max_issues_per_contributor_total', ''),
  ('earned_slots_enabled', 'false'),
  ('earned_slots_threshold', '2'),
  ('earned_slots_max', '3'),
  -- §3.5 slot-freeing definition - inert until the assignment slice
  ('qualifying_pr_requires_non_draft', 'true'),
  ('qualifying_pr_requires_ci_pass', 'true'),
  ('qualifying_pr_requires_issue_link', 'true'),
  ('qualifying_pr_min_meaningful_lines', '10'),
  -- §3.6 hard gates - inert until the assignment slice
  ('min_account_age_days', '90'),
  ('require_pre_announcement_commit', 'true'),
  ('min_pre_announcement_commits', '1'),
  ('block_bot_accounts', 'true'),
  ('block_issue_author', 'true'),
  ('block_org_members', 'true'),
  ('stale_assignment_days', '5'),
  ('abandons_before_lockout', '2'),
  ('voluntary_release_grace_hours', '48'),
  -- §3.7 application window and draw - inert until the assignment slice
  ('application_window_hours', '24'),
  ('empty_window_retries', '3'),
  ('fallback_to_first_come', 'true'),
  ('draw_order', 'randomised'),
  ('sequential_slot_consumption', 'true'),
  ('draw_from_weak_pool_if_empty', 'true'),
  -- §3.8 newcomer reservation - inert until the assignment slice
  ('newcomer_reservation_enabled', 'true'),
  ('reserved_pct_easy', '50'),
  ('reserved_pct_standard', '30'),
  ('reserved_pct_advanced', '0'),
  ('newcomer_definition', 'zero_completed_grainhack_issues'),
  ('reservation_fallback_to_open_pool', 'true'),
  -- §3.9 draw weights - inert until the assignment slice
  ('weight_fit_strong', '2.0'),
  ('weight_fit_plausible', '1.0'),
  ('weight_fit_weak', '0.25'),
  ('weight_difficulty_above', '0.5'),
  ('weight_prior_completion', '1.5'),
  ('weight_first_ever_application', '1.5'),
  ('weight_per_abandon', '0.5'),
  -- §3.10 judging and payout - inert until the judging slice
  ('units_rejected', '0'),
  ('units_accepted', '1'),
  ('units_substantial', '3'),
  ('units_exceptional', '5'),
  ('payout_floor', '50'),
  ('payout_floor_strategy', 'fund_highest_buckets_first'),
  ('duplicate_similarity_threshold', '0.92'),
  ('cross_check_enabled', 'true'),
  ('cross_check_provider', 'openai'),
  ('escalate_on_low_confidence', 'true'),
  ('maintainer_nominations_per_repo', '2'),
  ('appeal_window_days', '7'),
  -- §3.11 maintainer pool - inert until the payout slice
  ('maintainer_holdback_pct', '30'),
  ('maintainer_holdback_days', '90'),
  ('maintainer_min_repo_age_days', '90'),
  ('maintainer_criteria_weights', '{}'),
  -- §3.12 models - inert until the AI pipeline slices
  ('model_assignment', 'claude-sonnet-4-6'),
  ('model_judging', 'claude-sonnet-4-6'),
  ('model_cross_check', ''),
  ('model_escalation', ''),
  ('prompt_version_assignment', ''),
  ('prompt_version_judging', '')
ON CONFLICT DO NOTHING;
