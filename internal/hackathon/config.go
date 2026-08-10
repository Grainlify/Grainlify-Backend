// Package hackathon implements GrainHack (AI-specs.md) - a time-boxed
// hackathon run on the platform. This is Slice 1 ("Foundation"): the
// lifecycle mechanics for phases draft/application_period/issue_prep/live,
// project applications, and GitHub-label issue intake. The AI
// assignment/judging pipelines and payouts are future slices.
package hackathon

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// pgExecutor is the subset of db.DBPool that config reads/writes need.
// Satisfied structurally by both db.DBPool (standalone) and pgx.Tx (inside a
// transaction - see phase.go's Transition, which writes a phase change and
// its audit row atomically), mirroring internal/handlers/points.go's
// pgExecutor pattern.
type pgExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SettingDef is static metadata for one config key from AI-specs.md §3.
// Only Definitions' shape lives in Go; current values live in
// hackathon_config_settings (DB) so editing a description never needs a
// migration - see that table's migration comment for the full rationale.
type SettingDef struct {
	Key         string
	Type        string // "bool" | "int" | "float" | "string" | "enum" | "money" | "object"
	Default     string // string-encoded, matches how values are stored
	Section     string
	Description string
	ValidRange  string // human-readable; empty when not applicable
	// Active is false for settings with no consuming logic yet in this
	// slice (§3.4 onward) - stored and shown in the settings UI, so admins
	// can see/publish the full rule set early, but nothing reads them.
	Active bool
}

// Definitions is every config key from AI-specs.md §3.1-§3.12, in spec
// order. Keep in sync with migrations/000039_hackathon_config_settings.up.sql's
// seed INSERT - every key here must have a seeded row, and vice versa.
// SectionOrder is the canonical order §3's groups are presented in, shared
// by the admin settings UI and the public rules page so the two never drift.
var SectionOrder = []string{
	"Hackathon setup",
	"Issue intake",
	"Contributor slots and caps",
	"Slot-freeing definition",
	"Hard gates",
	"Application window and draw",
	"Newcomer reservation",
	"Draw weights",
	"Judging and payout",
	"Maintainer pool",
	"Chains",
	"Models",
}

var Definitions = map[string]SettingDef{
	// §3.2 hackathon setup - name/dates/prize pools are real hackathons
	// columns (per-hackathon only, no sensible global default), not
	// settings rows. merge_grace_period_hours is the one exception: it's
	// also a real hackathons column (set at creation) but kept here too so
	// admins can see/change the factory default in the generic settings UI.
	"merge_grace_period_hours": {Key: "merge_grace_period_hours", Type: "int", Default: "48", Section: "Hackathon setup", Description: "Hours after the hackathon ends a merge still counts, to avoid punishing contributors for maintainer review latency.", Active: true},

	// §3.3 issue intake - active in Slice 1
	"grainhack_label":               {Key: "grainhack_label", Type: "string", Default: "grainhack", Section: "Issue intake", Description: "The GitHub label that pulls an issue into the event.", Active: true},
	"max_issues_per_org":            {Key: "max_issues_per_org", Type: "int", Default: "50", Section: "Issue intake", Description: "Per-org cap on issues entered into one hackathon, enforced at label-sync time.", Active: true},
	"require_acceptance_criteria":   {Key: "require_acceptance_criteria", Type: "bool", Default: "true", Section: "Issue intake", Description: "Blocks publication until acceptance criteria are set.", Active: true},
	"require_difficulty_tier":       {Key: "require_difficulty_tier", Type: "bool", Default: "true", Section: "Issue intake", Description: "Blocks publication until a difficulty tier is set.", Active: true},
	"allow_late_issue_entry":        {Key: "allow_late_issue_entry", Type: "bool", Default: "true", Section: "Issue intake", Description: "Whether the GrainHack label can still be applied after the hackathon goes live.", Active: true},
	"late_entry_cutoff_hours":       {Key: "late_entry_cutoff_hours", Type: "int", Default: "48", Section: "Issue intake", Description: "Hours before the hackathon ends after which late issue entry is no longer allowed.", ValidRange: ">= 0", Active: true},
	"auto_revert_oob_assignment":    {Key: "auto_revert_oob_assignment", Type: "bool", Default: "true", Section: "Issue intake", Description: "Automatically remove and comment on GitHub-direct (out-of-band) assignments to GrainHack issues.", Active: true},
	"oob_assignment_flag_threshold": {Key: "oob_assignment_flag_threshold", Type: "int", Default: "3", Section: "Issue intake", Description: "Repeated out-of-band assignments from an org before it's flagged for admin review.", ValidRange: ">= 1", Active: true},

	// §3.4 contributor slots and caps - inert until the assignment slice
	"slots_per_contributor":              {Key: "slots_per_contributor", Type: "int", Default: "2", Section: "Contributor slots and caps", Description: "Concurrent open assignments allowed per contributor.", Active: true},
	"slot_freed_on":                      {Key: "slot_freed_on", Type: "enum", Default: "pr_submission", Section: "Contributor slots and caps", Description: "When an assignment's slot is freed.", ValidRange: "pr_submission | pr_merge", Active: true},
	"max_issues_per_contributor_per_org": {Key: "max_issues_per_contributor_per_org", Type: "int", Default: "4", Section: "Contributor slots and caps", Description: "Cap on issues one contributor can win from a single org, across the whole event.", Active: true},
	"max_issues_per_contributor_total":   {Key: "max_issues_per_contributor_total", Type: "int", Default: "", Section: "Contributor slots and caps", Description: "Cap on issues one contributor can win across the whole event. Empty = unlimited.", Active: true},
	"earned_slots_enabled":               {Key: "earned_slots_enabled", Type: "bool", Default: "false", Section: "Contributor slots and caps", Description: "Whether a 3rd slot can be earned after completing enough issues.", Active: true},
	"earned_slots_threshold":             {Key: "earned_slots_threshold", Type: "int", Default: "2", Section: "Contributor slots and caps", Description: "Completions needed to earn an extra slot.", Active: true},
	"earned_slots_max":                   {Key: "earned_slots_max", Type: "int", Default: "3", Section: "Contributor slots and caps", Description: "Ceiling on earned slots.", Active: true},
	// Not in AI-specs.md §3.4 - added as the answer to its own §13 open
	// question 1. Applications are free and slots are consumed only on
	// winning, so without a cap one farmer applies to every open issue and
	// dominates every draw pool at zero cost.
	"max_concurrent_applications": {Key: "max_concurrent_applications", Type: "int", Default: "5", Section: "Contributor slots and caps", Description: "Cap on a contributor's simultaneously-open applications. Applications are free; only a win consumes a slot.", ValidRange: ">= 1", Active: true},

	// §3.5 slot-freeing definition - inert until the assignment slice
	"qualifying_pr_requires_non_draft":   {Key: "qualifying_pr_requires_non_draft", Type: "bool", Default: "true", Section: "Slot-freeing definition", Description: "A draft PR does not free the contributor's slot.", Active: true},
	"qualifying_pr_requires_ci_pass":     {Key: "qualifying_pr_requires_ci_pass", Type: "bool", Default: "true", Section: "Slot-freeing definition", Description: "CI must pass for a PR to free the slot.", Active: true},
	"qualifying_pr_requires_issue_link":  {Key: "qualifying_pr_requires_issue_link", Type: "bool", Default: "true", Section: "Slot-freeing definition", Description: "The PR must link the GrainHack issue to free the slot.", Active: true},
	"qualifying_pr_min_meaningful_lines": {Key: "qualifying_pr_min_meaningful_lines", Type: "int", Default: "10", Section: "Slot-freeing definition", Description: "Minimum meaningful lines changed (excludes generated/lockfile/formatting) for a PR to count.", ValidRange: ">= 0", Active: true},

	// §3.6 hard gates - inert until the assignment slice
	"min_account_age_days":            {Key: "min_account_age_days", Type: "int", Default: "90", Section: "Hard gates", Description: "Minimum GitHub account age, measured before announced_at.", ValidRange: ">= 0", Active: true},
	"require_pre_announcement_commit": {Key: "require_pre_announcement_commit", Type: "bool", Default: "true", Section: "Hard gates", Description: "Require at least one commit before announced_at.", Active: true},
	"min_pre_announcement_commits":    {Key: "min_pre_announcement_commits", Type: "int", Default: "1", Section: "Hard gates", Description: "Existence check, not a volume check.", ValidRange: ">= 0", Active: true},
	"block_bot_accounts":              {Key: "block_bot_accounts", Type: "bool", Default: "true", Section: "Hard gates", Description: "Reject applications from GitHub accounts of type Bot.", Active: true},
	"block_issue_author":              {Key: "block_issue_author", Type: "bool", Default: "true", Section: "Hard gates", Description: "The issue's own author cannot win it.", Active: true},
	"block_org_members":               {Key: "block_org_members", Type: "bool", Default: "true", Section: "Hard gates", Description: "Members of the issue's own org cannot win it.", Active: true},
	"stale_assignment_days":           {Key: "stale_assignment_days", Type: "int", Default: "5", Section: "Hard gates", Description: "Days with no qualifying PR before an assignment auto-releases.", ValidRange: ">= 1", Active: true},
	"abandons_before_lockout":         {Key: "abandons_before_lockout", Type: "int", Default: "2", Section: "Hard gates", Description: "Abandons in one event before a contributor is locked out of new assignments.", ValidRange: ">= 1", Active: true},
	"voluntary_release_grace_hours":   {Key: "voluntary_release_grace_hours", Type: "int", Default: "48", Section: "Hard gates", Description: "Releasing inside this window after assignment doesn't count as an abandon.", ValidRange: ">= 0", Active: true},

	// §3.7 application window and draw - inert until the assignment slice
	"application_window_hours":    {Key: "application_window_hours", Type: "int", Default: "24", Section: "Application window and draw", Description: "How long an issue accepts applications before the draw runs. Shorten for short events.", ValidRange: ">= 1", Active: true},
	"empty_window_retries":        {Key: "empty_window_retries", Type: "int", Default: "3", Section: "Application window and draw", Description: "Retries before falling back to first-come assignment.", ValidRange: ">= 0", Active: true},
	"fallback_to_first_come":      {Key: "fallback_to_first_come", Type: "bool", Default: "true", Section: "Application window and draw", Description: "Fall back to first-come after retries are exhausted.", Active: true},
	"draw_order":                  {Key: "draw_order", Type: "enum", Default: "randomised", Section: "Application window and draw", Description: "Order issues are processed in during the draw. Declared, not yet consumed - the draw currently runs issues in a fixed order.", ValidRange: "randomised"},
	"sequential_slot_consumption": {Key: "sequential_slot_consumption", Type: "bool", Default: "true", Section: "Application window and draw", Description: "A win consumes a slot before the next issue is drawn. Declared, not yet consumed - slot freeing currently always uses the same rule."},
	// Not in AI-specs.md §3.7 - added because an exact live applicant count
	// rewards applying late (see migration 000043). "bucketed" shows a
	// coarse band instead, keeping the anti-crowding signal without the
	// precision advantage.
	"applicant_count_visibility":   {Key: "applicant_count_visibility", Type: "enum", Default: "bucketed", Section: "Application window and draw", Description: "How much of an issue's applicant pool contributors see while the window is open.", ValidRange: "hidden | bucketed | exact", Active: true},
	"draw_from_weak_pool_if_empty": {Key: "draw_from_weak_pool_if_empty", Type: "bool", Default: "true", Section: "Application window and draw", Description: "Draw from weak-fit applicants rather than leave an issue unassigned.", Active: true},

	// §3.8 newcomer reservation - inert until the assignment slice
	"newcomer_reservation_enabled":      {Key: "newcomer_reservation_enabled", Type: "bool", Default: "true", Section: "Newcomer reservation", Description: "Reserve a share of issues for newcomers.", Active: true},
	"reserved_pct_easy":                 {Key: "reserved_pct_easy", Type: "int", Default: "50", Section: "Newcomer reservation", Description: "% of easy issues reserved for newcomers.", ValidRange: "0-100", Active: true},
	"reserved_pct_standard":             {Key: "reserved_pct_standard", Type: "int", Default: "30", Section: "Newcomer reservation", Description: "% of standard issues reserved for newcomers.", ValidRange: "0-100", Active: true},
	"reserved_pct_advanced":             {Key: "reserved_pct_advanced", Type: "int", Default: "0", Section: "Newcomer reservation", Description: "% of advanced issues reserved for newcomers.", ValidRange: "0-100", Active: true},
	"newcomer_definition":               {Key: "newcomer_definition", Type: "enum", Default: "zero_completed_grainhack_issues", Section: "Newcomer reservation", Description: "What counts as a newcomer. Declared, not yet consumed - the reservation logic uses its own newcomer test.", ValidRange: "zero_completed_grainhack_issues"},
	"reservation_fallback_to_open_pool": {Key: "reservation_fallback_to_open_pool", Type: "bool", Default: "true", Section: "Newcomer reservation", Description: "Fall back to the open pool if no newcomers applied to a reserved issue.", Active: true},

	// §3.9 draw weights - inert until the assignment slice
	"weight_fit_strong":             {Key: "weight_fit_strong", Type: "float", Default: "2.0", Section: "Draw weights", Description: "Ticket multiplier for a 'strong' AI fit assessment.", Active: true},
	"weight_fit_plausible":          {Key: "weight_fit_plausible", Type: "float", Default: "1.0", Section: "Draw weights", Description: "Ticket multiplier for a 'plausible' AI fit assessment (the expected default for most newcomers).", Active: true},
	"weight_fit_weak":               {Key: "weight_fit_weak", Type: "float", Default: "0.25", Section: "Draw weights", Description: "Ticket multiplier for a 'weak' AI fit assessment.", Active: true},
	"weight_difficulty_above":       {Key: "weight_difficulty_above", Type: "float", Default: "0.5", Section: "Draw weights", Description: "Ticket multiplier when demonstrated skill is below the issue's difficulty tier.", Active: true},
	"weight_prior_completion":       {Key: "weight_prior_completion", Type: "float", Default: "1.5", Section: "Draw weights", Description: "Ticket multiplier per prior completed GrainHack issue.", Active: true},
	"weight_first_ever_application": {Key: "weight_first_ever_application", Type: "float", Default: "1.5", Section: "Draw weights", Description: "Ticket multiplier for a contributor who has never been assigned a GrainHack issue. Held on every application until they win one, so applying to more issues never costs them the bonus.", Active: true},
	"weight_per_abandon":            {Key: "weight_per_abandon", Type: "float", Default: "0.5", Section: "Draw weights", Description: "Ticket multiplier per prior abandon (< 1.0, i.e. a penalty).", Active: true},

	// §3.10 judging and payout - inert until the judging slice
	"units_rejected":                  {Key: "units_rejected", Type: "int", Default: "0", Section: "Judging and payout", Description: "Payout units for a 'rejected' PR bucket.", Active: true},
	"units_accepted":                  {Key: "units_accepted", Type: "int", Default: "1", Section: "Judging and payout", Description: "Payout units for an 'accepted' PR bucket.", Active: true},
	"units_substantial":               {Key: "units_substantial", Type: "int", Default: "3", Section: "Judging and payout", Description: "Payout units for a 'substantial' PR bucket.", Active: true},
	"units_exceptional":               {Key: "units_exceptional", Type: "int", Default: "5", Section: "Judging and payout", Description: "Payout units for an 'exceptional' PR bucket.", Active: true},
	"payout_floor":                    {Key: "payout_floor", Type: "money", Default: "50", Section: "Judging and payout", Description: "Minimum payout; below this, payout_floor_strategy decides how the pool is allocated.", Active: true},
	"payout_floor_strategy":           {Key: "payout_floor_strategy", Type: "enum", Default: "fund_highest_buckets_first", Section: "Judging and payout", Description: "How the pool is allocated when unit_value would fall below the floor.", ValidRange: "fund_highest_buckets_first", Active: true},
	"diminishing_returns_enabled":     {Key: "diminishing_returns_enabled", Type: "bool", Default: "true", Section: "Judging and payout", Description: "Reduce the payout of each additional accepted PR from the same contributor, so the pool spreads across more people.", Active: true},
	"diminishing_returns_curve":       {Key: "diminishing_returns_curve", Type: "list", Default: "[1.0, 0.8, 0.6, 0.5, 0.4]", Section: "Judging and payout", Description: "Multiplier applied to a contributor's 1st, 2nd, 3rd... accepted PR, ordered by merge time. The last value repeats for every further PR. Positions count only funded PRs, so a rejected one does not use up a place.", Active: true},
	"duplicate_similarity_threshold":  {Key: "duplicate_similarity_threshold", Type: "float", Default: "0.92", Section: "Judging and payout", Description: "Embedding cosine-similarity threshold for flagging duplicate PRs. Declared, not yet consumed - FindDuplicates takes a threshold but stage 2 is not driven yet.", ValidRange: "0-1"},
	"cross_check_enabled":             {Key: "cross_check_enabled", Type: "bool", Default: "true", Section: "Judging and payout", Description: "Run every judged PR through a second AI provider."},
	"cross_check_provider":            {Key: "cross_check_provider", Type: "enum", Default: "openai", Section: "Judging and payout", Description: "The second provider used for cross-checking."},
	"escalate_on_low_confidence":      {Key: "escalate_on_low_confidence", Type: "bool", Default: "true", Section: "Judging and payout", Description: "Route low-confidence verdicts to human escalation."},
	"maintainer_nominations_per_repo": {Key: "maintainer_nominations_per_repo", Type: "int", Default: "2", Section: "Judging and payout", Description: "Cap on maintainer 'exceptional' nominations per repo.", ValidRange: ">= 0"},
	"appeal_window_days":              {Key: "appeal_window_days", Type: "int", Default: "7", Section: "Judging and payout", Description: "Days the appeal window stays open after results are published.", ValidRange: ">= 0", Active: true},

	// §3.11 maintainer pool - inert until the payout slice
	"maintainer_holdback_pct":      {Key: "maintainer_holdback_pct", Type: "int", Default: "30", Section: "Maintainer pool", Description: "% of a maintainer's payout held back.", ValidRange: "0-100", Active: true},
	"maintainer_holdback_days":     {Key: "maintainer_holdback_days", Type: "int", Default: "90", Section: "Maintainer pool", Description: "Days the holdback is held before release.", Active: true},
	"maintainer_min_repo_age_days": {Key: "maintainer_min_repo_age_days", Type: "int", Default: "90", Section: "Maintainer pool", Description: "Minimum repo age to be maintainer-pool eligible."},
	"maintainer_criteria_weights":  {Key: "maintainer_criteria_weights", Type: "object", Default: "{}", Section: "Maintainer pool", Description: "Weights for each maintainer-pool eligibility criterion. Unmentioned criteria keep their default; a criterion dropped for thin data has its weight redistributed across the rest.", Active: true},
	// §7's holdback is only an anti-farming mechanism if its release is
	// conditional on the thing it measures - "a maintainer farming an event is
	// gone the next day; one genuinely growing a project is still there". A
	// timer alone pays the farmer three months late.
	"maintainer_activity_window_days":   {Key: "maintainer_activity_window_days", Type: "int", Default: "60", Section: "Maintainer pool", Description: "Days after the event closes over which continued repo activity is measured for holdback release.", ValidRange: ">= 1", Active: true},
	"maintainer_activity_full_commits":  {Key: "maintainer_activity_full_commits", Type: "int", Default: "5", Section: "Maintainer pool", Description: "Commits in the activity window that qualify for full holdback release.", ValidRange: ">= 1", Active: true},
	"maintainer_activity_full_prs":      {Key: "maintainer_activity_full_prs", Type: "int", Default: "2", Section: "Maintainer pool", Description: "Merged PRs in the activity window that qualify for full holdback release.", ValidRange: ">= 1", Active: true},
	"maintainer_partial_release_pct":    {Key: "maintainer_partial_release_pct", Type: "int", Default: "50", Section: "Maintainer pool", Description: "% of the holdback released when a repo shows some activity but below the full-release bar.", ValidRange: "0-100", Active: true},
	"maintainer_withheld_destination":   {Key: "maintainer_withheld_destination", Type: "enum", Default: "next_event_pool", Section: "Maintainer pool", Description: "Where a withheld holdback goes. Published in advance so it is a decision rather than an accident.", ValidRange: "next_event_pool|returned_to_treasury", Active: true},
	"unclaimed_sweep_days":              {Key: "unclaimed_sweep_days", Type: "int", Default: "180", Section: "Chains", Description: "Days after settlement before unclaimed on-chain funds may be swept. Sweeping is time-locked and multisig-gated.", ValidRange: ">= 1"},
	"unclaimed_sweep_destination":       {Key: "unclaimed_sweep_destination", Type: "enum", Default: "next_event_pool_same_chain", Section: "Chains", Description: "Where unclaimed funds go, always on the same chain. Published in advance.", ValidRange: "next_event_pool_same_chain|refund_to_sponsor"},
	"empty_chain_pool_disposition":      {Key: "empty_chain_pool_disposition", Type: "enum", Default: "refund_to_sponsor", Section: "Chains", Description: "What happens to a chain's pool that ends with no accepted PRs. Refunded to that chain's sponsor via the same multisig, time-locked path as a cancelled event - not a separate mechanism.", ValidRange: "refund_to_sponsor|next_event_pool_same_chain"},
	"contract_upgrade_policy":           {Key: "contract_upgrade_policy", Type: "enum", Default: "multisig_timelock_exceeding_claim_window", Section: "Chains", Description: "Upgrades require multisig plus a timelock longer than the claim window, so a contributor always has time to exit before any change takes effect.", ValidRange: "multisig_timelock_exceeding_claim_window|immutable"},
	"maintainer_clarity_min_ratings":    {Key: "maintainer_clarity_min_ratings", Type: "int", Default: "3", Section: "Maintainer pool", Description: "Ratings a repo needs before issue clarity counts toward its score; below this the criterion is dropped and the remaining weights renormalised.", ValidRange: ">= 1", Active: true},
	"maintainer_first_timers_reference": {Key: "maintainer_first_timers_reference", Type: "int", Default: "200", Section: "Maintainer pool", Description: "Distinct first-time contributors that score full marks. Log-scaled, so smaller repos still separate from each other rather than all saturating at a low cap.", ValidRange: ">= 2", Active: true},

	// §3.12 models - inert until the AI pipeline slices
	// Layer 2 (§4.3) is the only non-deterministic step in the assignment
	// pipeline. Off by default: every applicant is assessed "plausible",
	// which is a valid §4.4 outcome, so gates/tickets/draws stay fully
	// exercisable with zero model calls.
	"ai_fit_assessment_enabled": {Key: "ai_fit_assessment_enabled", Type: "bool", Default: "false", Section: "Models", Description: "Run the AI fit assessment (§4.3). When off, every applicant that passes the hard gates is assessed 'plausible'.", Active: true},
	// Gates §5 stages 3-5 (judge, cross-check, escalation). Stages 1, 2
	// and 6 - pre-filter, duplicate detection and payout maths - are
	// deterministic and run regardless.
	"ai_judging_enabled": {Key: "ai_judging_enabled", Type: "bool", Default: "false", Section: "Models", Description: "Run the AI judging stages (§5.3-5.5). When off, PRs are pre-filtered and duplicate-checked but not bucketed by a model.", Active: true},
	// §12: "Event 1 - shadow mode. Judging runs but publishes nothing; pay
	// by hand." On by default so that is what happens unless someone
	// deliberately turns it off.
	"judging_shadow_mode":       {Key: "judging_shadow_mode", Type: "bool", Default: "true", Section: "Judging and payout", Description: "Compute verdicts and payouts but publish nothing to contributors. The rollout's first event is meant to run this way.", Active: true},
	"model_assignment":          {Key: "model_assignment", Type: "string", Default: "claude-sonnet-4-6", Section: "Models", Description: "Model used for the per-applicant AI fit assessment.", Active: true},
	"model_judging":             {Key: "model_judging", Type: "string", Default: "claude-sonnet-4-6", Section: "Models", Description: "Model used for the per-PR judging call.", Active: true},
	"model_cross_check":         {Key: "model_cross_check", Type: "string", Default: "", Section: "Models", Description: "Model used by the cross-check provider.", Active: true},
	"model_escalation":          {Key: "model_escalation", Type: "string", Default: "", Section: "Models", Description: "Model used for escalated (disagreement) verdicts.", Active: true},
	"prompt_version_assignment": {Key: "prompt_version_assignment", Type: "string", Default: "", Section: "Models", Description: "Assignment prompt version, tracked and logged per call.", Active: true},
	"prompt_version_judging":    {Key: "prompt_version_judging", Type: "string", Default: "", Section: "Models", Description: "Judging prompt version, tracked and logged per call."},
}

// DefaultValue returns the factory default for key, or "" if key is unknown.
func DefaultValue(key string) string {
	if def, ok := Definitions[key]; ok {
		return def.Default
	}
	return ""
}

// EffectiveValue resolves key for hackathonID (nil = global scope only):
// per-hackathon override > global default row > factory default.
// pool only needs to satisfy pgExecutor (QueryRow) here, not the full
// db.DBPool - narrower on purpose so SetValue can reuse this to compute a
// real "prior effective value" for its audit row (below) using the same
// pgExecutor it was already given, standalone or inside a tx. Every
// existing db.DBPool-typed caller keeps compiling unchanged, since
// db.DBPool already satisfies this narrower interface structurally.
func EffectiveValue(ctx context.Context, pool pgExecutor, hackathonID *uuid.UUID, key string) (string, error) {
	if hackathonID != nil {
		var v string
		err := pool.QueryRow(ctx, `SELECT value FROM hackathon_config_settings WHERE hackathon_id = $1 AND key = $2`, *hackathonID, key).Scan(&v)
		if err == nil {
			return v, nil
		}
		if err != pgx.ErrNoRows {
			return "", fmt.Errorf("hackathon.EffectiveValue: query override: %w", err)
		}
	}
	var v string
	err := pool.QueryRow(ctx, `SELECT value FROM hackathon_config_settings WHERE hackathon_id IS NULL AND key = $1`, key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if err != pgx.ErrNoRows {
		return "", fmt.Errorf("hackathon.EffectiveValue: query global: %w", err)
	}
	return DefaultValue(key), nil
}

// EffectiveValues resolves every known key for hackathonID in one query pair
// (used by the settings-list endpoint and the phase-3 config snapshot).
func EffectiveValues(ctx context.Context, pool db.DBPool, hackathonID *uuid.UUID) (map[string]string, error) {
	out := make(map[string]string, len(Definitions))
	for key, def := range Definitions {
		out[key] = def.Default
	}

	globalRows, err := pool.Query(ctx, `SELECT key, value FROM hackathon_config_settings WHERE hackathon_id IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("hackathon.EffectiveValues: query global: %w", err)
	}
	defer globalRows.Close()
	for globalRows.Next() {
		var k, v string
		if err := globalRows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("hackathon.EffectiveValues: scan global: %w", err)
		}
		out[k] = v
	}
	if err := globalRows.Err(); err != nil {
		return nil, err
	}

	if hackathonID != nil {
		overrideRows, err := pool.Query(ctx, `SELECT key, value FROM hackathon_config_settings WHERE hackathon_id = $1`, *hackathonID)
		if err != nil {
			return nil, fmt.Errorf("hackathon.EffectiveValues: query overrides: %w", err)
		}
		defer overrideRows.Close()
		for overrideRows.Next() {
			var k, v string
			if err := overrideRows.Scan(&k, &v); err != nil {
				return nil, fmt.Errorf("hackathon.EffectiveValues: scan overrides: %w", err)
			}
			out[k] = v
		}
		if err := overrideRows.Err(); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// SetValue writes key's new value at the given scope (hackathonID nil =
// global default) and records the change in config_audit in the same call,
// reading the prior effective value first so the audit row has a real
// old_value even the first time a key is overridden.
func SetValue(ctx context.Context, exec pgExecutor, hackathonID *uuid.UUID, key, newValue string, actorID uuid.UUID) error {
	if _, ok := Definitions[key]; !ok {
		return fmt.Errorf("hackathon.SetValue: unknown config key %q", key)
	}

	// The real prior EFFECTIVE value (override > global > factory default),
	// not just "was there already a row at this exact scope" - so the audit
	// row reads as a true value transition (e.g. "48 -> 24") even the very
	// first time a key is ever touched, matching what the setting actually
	// looked like to anyone reading it before this change.
	oldStr, err := EffectiveValue(ctx, exec, hackathonID, key)
	if err != nil {
		return fmt.Errorf("hackathon.SetValue: read old value: %w", err)
	}

	// Two distinct partial unique indexes exist (global vs. per-hackathon
	// scope - see the table's migration), so the ON CONFLICT arbiter must
	// be chosen upfront to match whichever one the incoming row's
	// hackathon_id actually qualifies for. Picking the wrong one raises a
	// real constraint-violation error rather than a graceful no-op, and -
	// critically, when exec is a pgx.Tx - poisons the whole transaction, so
	// this can't be a try-then-fallback-on-error pattern.
	var writeErr error
	if hackathonID == nil {
		_, writeErr = exec.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by, updated_at)
VALUES (NULL, $1, $2, $3, now())
ON CONFLICT (key) WHERE hackathon_id IS NULL DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
`, key, newValue, actorID)
	} else {
		_, writeErr = exec.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (hackathon_id, key) WHERE hackathon_id IS NOT NULL DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
`, *hackathonID, key, newValue, actorID)
	}
	if writeErr != nil {
		return fmt.Errorf("hackathon.SetValue: write value: %w", writeErr)
	}

	if _, err := exec.Exec(ctx, `
INSERT INTO config_audit (hackathon_id, key, old_value, new_value, actor_user_id)
VALUES ($1, $2, $3, $4, $5)
`, hackathonID, key, oldStr, newValue, actorID); err != nil {
		return fmt.Errorf("hackathon.SetValue: write audit: %w", err)
	}

	return nil
}

// ResetValue deletes a per-hackathon override so the key falls back to the
// global default, recording the reset in config_audit. A no-op (but still
// audited) if no override existed.
func ResetValue(ctx context.Context, exec pgExecutor, hackathonID uuid.UUID, key string, actorID uuid.UUID) error {
	if _, ok := Definitions[key]; !ok {
		return fmt.Errorf("hackathon.ResetValue: unknown config key %q", key)
	}

	var oldValue *string
	err := exec.QueryRow(ctx, `SELECT value FROM hackathon_config_settings WHERE hackathon_id = $1 AND key = $2`, hackathonID, key).Scan(&oldValue)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("hackathon.ResetValue: read old value: %w", err)
	}
	if oldValue == nil {
		return nil // nothing to reset
	}

	if _, err := exec.Exec(ctx, `DELETE FROM hackathon_config_settings WHERE hackathon_id = $1 AND key = $2`, hackathonID, key); err != nil {
		return fmt.Errorf("hackathon.ResetValue: delete override: %w", err)
	}

	// New effective value after the delete is whatever the global scope
	// resolves to - a global override row, or the factory default.
	var globalValue *string
	if err := exec.QueryRow(ctx, `SELECT value FROM hackathon_config_settings WHERE hackathon_id IS NULL AND key = $1`, key).Scan(&globalValue); err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("hackathon.ResetValue: read global value: %w", err)
	}
	newValue := DefaultValue(key)
	if globalValue != nil {
		newValue = *globalValue
	}

	if _, err := exec.Exec(ctx, `
INSERT INTO config_audit (hackathon_id, key, old_value, new_value, actor_user_id)
VALUES ($1, $2, $3, $4, $5)
`, hackathonID, key, *oldValue, newValue, actorID); err != nil {
		return fmt.Errorf("hackathon.ResetValue: write audit: %w", err)
	}
	return nil
}
