package hackathon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// GateResult is Layer 1's verdict for one application (AI-specs.md §4.1).
// Failed carries the specific reason shown to the applicant - "Any failure
// rejects the application with the specific reason shown to the applicant."
type GateResult struct {
	Passed bool   `json:"passed"`
	Gate   string `json:"gate,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func gatePass() GateResult { return GateResult{Passed: true} }

func gateFail(gate, format string, args ...any) GateResult {
	return GateResult{Passed: false, Gate: gate, Reason: fmt.Sprintf(format, args...)}
}

// ApplicantContext is everything the gates need about who is applying to
// what. Assembled by the caller (the apply handler) so the gate logic itself
// is pure DB/GitHub reads and stays directly testable.
type ApplicantContext struct {
	HackathonID  uuid.UUID
	IssueID      uuid.UUID // hackathon_issues.id
	ProjectID    uuid.UUID
	IssueNumber  int
	OrgLogin     string
	RepoFullName string
	UserID       uuid.UUID
	GitHubLogin  string
	// IssueAuthorLogin is the GitHub login that opened the issue; empty if
	// unknown, which disables the block_issue_author gate rather than
	// guessing.
	IssueAuthorLogin string
	// SearchToken is the *applicant's own* decrypted OAuth token, used only
	// for the pre-event-activity gate.
	//
	// Deliberately not the GitHub App installation token that the other
	// GitHub calls here use. Search results are scoped to what the token
	// can see, and an installation token can only see repos the app is
	// installed on - so `author:<applicant>` would return ~0 for almost
	// every real contributor. That is a *determinate* answer and a wrong
	// one, which would reject legitimate applicants rather than merely
	// skipping the check. The applicant's own token has full public search
	// scope, so the answer is both determinate and correct.
	SearchToken string
}

// gateEnv is the resolved config + hackathon state one CheckGates call needs,
// loaded once so each gate is a cheap comparison rather than its own query.
type gateEnv struct {
	announcedAt              time.Time
	minAccountAgeDays        int
	requirePreAnnounceCommit bool
	minPreAnnounceCommits    int
	blockBots                bool
	blockIssueAuthor         bool
	blockOrgMembers          bool
	slotsPerContributor      int
	earnedSlotsEnabled       bool
	earnedSlotsThreshold     int
	earnedSlotsMax           int
	maxPerOrg                int
	maxTotal                 int // 0 = unlimited (config's empty string)
	abandonsBeforeLockout    int
	maxConcurrentApps        int
}

func loadGateEnv(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (*gateEnv, error) {
	vals, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.loadGateEnv: config: %w", err)
	}
	var announced *time.Time
	if err := pool.QueryRow(ctx, `SELECT announced_at FROM hackathons WHERE id = $1`, hackathonID).Scan(&announced); err != nil {
		return nil, fmt.Errorf("hackathon.loadGateEnv: load hackathon: %w", err)
	}
	if announced == nil {
		// §3.2 marks announced_at "load-bearing": account-age and repo-history
		// gates are measured against it. Reaching Phase 1 requires it, so a
		// nil here means data was edited out from under a live event.
		return nil, fmt.Errorf("hackathon.loadGateEnv: hackathon %s has no announced_at, which the age gates measure against", hackathonID)
	}
	return &gateEnv{
		announcedAt:              *announced,
		minAccountAgeDays:        atoiOr(vals["min_account_age_days"], 90),
		requirePreAnnounceCommit: vals["require_pre_announcement_commit"] == "true",
		minPreAnnounceCommits:    atoiOr(vals["min_pre_announcement_commits"], 1),
		blockBots:                vals["block_bot_accounts"] == "true",
		blockIssueAuthor:         vals["block_issue_author"] == "true",
		blockOrgMembers:          vals["block_org_members"] == "true",
		slotsPerContributor:      atoiOr(vals["slots_per_contributor"], 2),
		earnedSlotsEnabled:       vals["earned_slots_enabled"] == "true",
		earnedSlotsThreshold:     atoiOr(vals["earned_slots_threshold"], 2),
		earnedSlotsMax:           atoiOr(vals["earned_slots_max"], 3),
		maxPerOrg:                atoiOr(vals["max_issues_per_contributor_per_org"], 4),
		maxTotal:                 atoiOr(vals["max_issues_per_contributor_total"], 0),
		abandonsBeforeLockout:    atoiOr(vals["abandons_before_lockout"], 2),
		maxConcurrentApps:        atoiOr(vals["max_concurrent_applications"], 5),
	}, nil
}

func atoiOr(s string, fallback int) int {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func atofOr(s string, fallback float64) float64 {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fallback
	}
	return f
}

// EffectiveSlots is the contributor's slot count for this event: the base
// slots_per_contributor, plus one earned slot once they've completed
// earned_slots_threshold issues, capped at earned_slots_max (§3.4).
// exec is narrowed to pgExecutor (not db.DBPool) so the draw can call this
// inside its slot-consumption transaction - see commitAssignment.
func EffectiveSlots(ctx context.Context, exec pgExecutor, hackathonID, userID uuid.UUID, env *gateEnv) (int, error) {
	slots := env.slotsPerContributor
	if !env.earnedSlotsEnabled {
		return slots, nil
	}
	var completed int
	if err := exec.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND status = 'completed'
`, hackathonID, userID).Scan(&completed); err != nil {
		return 0, fmt.Errorf("hackathon.EffectiveSlots: %w", err)
	}
	if env.earnedSlotsThreshold > 0 && completed >= env.earnedSlotsThreshold {
		slots++
	}
	if slots > env.earnedSlotsMax {
		slots = env.earnedSlotsMax
	}
	return slots, nil
}

// CheckGates runs AI-specs.md §4.1's Layer 1 in table order. Pure code, no
// AI, no randomness. Evaluated at application time; the draw (§4.5) re-runs
// the slot/cap subset at win time because state moves between the two.
//
// gh may be nil, which skips the GitHub-backed gates (account age, bot
// check, org membership) rather than failing the application - the same
// best-effort posture the rest of this package takes toward GitHub. The
// checks that protect against self-dealing and farming and can be answered
// from our own database are never skipped.
func CheckGates(
	ctx context.Context,
	pool db.DBPool,
	gh *github.Client,
	accessToken string,
	ac ApplicantContext,
) (GateResult, error) {
	env, err := loadGateEnv(ctx, pool, ac.HackathonID)
	if err != nil {
		return GateResult{}, err
	}

	// Gate: hackathon context. The issue must be published into a live
	// hackathon on an accepted project. Anything else and there is nothing
	// to apply to.
	var issueStatus, phase string
	var issueOrg string
	err = pool.QueryRow(ctx, `
SELECT hi.status, h.phase, hi.org_login
FROM hackathon_issues hi
JOIN hackathons h ON h.id = hi.hackathon_id
WHERE hi.id = $1 AND hi.hackathon_id = $2
`, ac.IssueID, ac.HackathonID).Scan(&issueStatus, &phase, &issueOrg)
	if err != nil {
		return gateFail("hackathon_context", "This issue is not part of a GrainHack."), nil
	}
	if phase != "live" {
		return gateFail("hackathon_context", "This GrainHack is not accepting applications right now."), nil
	}
	if issueStatus != "published" {
		return gateFail("hackathon_context", "This issue is not open for applications yet."), nil
	}

	// Gate: application window. Outside it there is no draw to enter.
	var windowOpens, windowCloses *time.Time
	if err := pool.QueryRow(ctx, `
SELECT application_window_opens_at, application_window_closes_at FROM hackathon_issues WHERE id = $1
`, ac.IssueID).Scan(&windowOpens, &windowCloses); err != nil {
		return GateResult{}, fmt.Errorf("hackathon.CheckGates: load window: %w", err)
	}
	now := time.Now().UTC()
	if windowOpens != nil && now.Before(*windowOpens) {
		return gateFail("application_window", "Applications for this issue open at %s.", windowOpens.UTC().Format(time.RFC3339)), nil
	}
	if windowCloses != nil && now.After(*windowCloses) {
		return gateFail("application_window", "Applications for this issue closed at %s.", windowCloses.UTC().Format(time.RFC3339)), nil
	}

	// Gate: self-assignment (issue author). §4.1's "cannot win your own
	// issue" half.
	if env.blockIssueAuthor && ac.IssueAuthorLogin != "" &&
		strings.EqualFold(ac.IssueAuthorLogin, ac.GitHubLogin) {
		return gateFail("block_issue_author", "You opened this issue, so you can't also be assigned to it."), nil
	}

	// Gate: abandon lockout.
	var abandons int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND abandon_recorded
`, ac.HackathonID, ac.UserID).Scan(&abandons); err != nil {
		return GateResult{}, fmt.Errorf("hackathon.CheckGates: count abandons: %w", err)
	}
	if abandons >= env.abandonsBeforeLockout {
		return gateFail("abandon_lockout", "You've had %d assignment(s) time out in this event, which is the limit.", abandons), nil
	}

	// Gate: slot availability. holds_slot (not status) is the source of
	// truth - with slot_freed_on = pr_submission a slot frees while the
	// assignment is still open.
	slots, err := EffectiveSlots(ctx, pool, ac.HackathonID, ac.UserID, env)
	if err != nil {
		return GateResult{}, err
	}
	var held int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND holds_slot
`, ac.HackathonID, ac.UserID).Scan(&held); err != nil {
		return GateResult{}, fmt.Errorf("hackathon.CheckGates: count held slots: %w", err)
	}
	if held >= slots {
		return gateFail("slot_availability", "You're holding %d of %d assignment slots. Submit a PR to free one.", held, slots), nil
	}

	// Gate: org cap - issues won from this org across the whole event.
	var wonFromOrg int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND org_login = $3
`, ac.HackathonID, ac.UserID, ac.OrgLogin).Scan(&wonFromOrg); err != nil {
		return GateResult{}, fmt.Errorf("hackathon.CheckGates: count org wins: %w", err)
	}
	if env.maxPerOrg > 0 && wonFromOrg >= env.maxPerOrg {
		return gateFail("org_cap", "You've already been assigned %d issue(s) from %s, which is the per-org limit.", wonFromOrg, ac.OrgLogin), nil
	}

	// Gate: total cap (optional; config's empty value means unlimited).
	if env.maxTotal > 0 {
		var wonTotal int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments WHERE hackathon_id = $1 AND user_id = $2
`, ac.HackathonID, ac.UserID).Scan(&wonTotal); err != nil {
			return GateResult{}, fmt.Errorf("hackathon.CheckGates: count total wins: %w", err)
		}
		if wonTotal >= env.maxTotal {
			return gateFail("total_cap", "You've already been assigned %d issue(s) in this event, which is the limit.", wonTotal), nil
		}
	}

	// Gate: concurrent open applications (§13 #1). Applications are free and
	// only a win consumes a slot, so without this one account can enter
	// every pool at no cost.
	if env.maxConcurrentApps > 0 {
		var open int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM hackathon_issue_applications
WHERE hackathon_id = $1 AND user_id = $2 AND status = 'applied'
`, ac.HackathonID, ac.UserID).Scan(&open); err != nil {
			return GateResult{}, fmt.Errorf("hackathon.CheckGates: count open applications: %w", err)
		}
		if open >= env.maxConcurrentApps {
			return gateFail("max_concurrent_applications", "You have %d open applications, which is the limit. Wait for a draw to resolve before applying to more.", open), nil
		}
	}

	// GitHub-backed gates. Best-effort: without a client we keep the
	// DB-answerable protections above rather than rejecting everyone.
	if gh == nil {
		return gatePass(), nil
	}

	pu, err := gh.GetPublicUser(ctx, accessToken, ac.GitHubLogin)
	if err == nil {
		if env.blockBots && strings.EqualFold(pu.Type, "Bot") {
			return gateFail("block_bot_accounts", "Bot accounts can't be assigned GrainHack issues."), nil
		}
		if env.minAccountAgeDays > 0 && !pu.CreatedAt.IsZero() {
			cutoff := env.announcedAt.AddDate(0, 0, -env.minAccountAgeDays)
			if pu.CreatedAt.After(cutoff) {
				return gateFail("min_account_age", "Your GitHub account must be at least %d days old as of this event's announcement.", env.minAccountAgeDays), nil
			}
		}
	}

	// Gate: self-assignment (org member). §4.1: "a maintainer cannot win
	// their own org's issues from any account visible as an org member."
	// An indeterminate answer (token can't see private membership) is
	// treated as not-a-member and logged by the caller - failing closed
	// here would block every applicant to any org whose membership we
	// can't read.
	if env.blockOrgMembers && ac.OrgLogin != "" {
		if member, err := gh.IsOrgMember(ctx, accessToken, ac.OrgLogin, ac.GitHubLogin); err == nil && member {
			return gateFail("block_org_members", "You're a member of %s, so you can't be assigned its GrainHack issues.", ac.OrgLogin), nil
		}
	}

	// Gate: pre-event activity. §3.6 is explicit that this is an existence
	// check, not a volume check - "≥1 commit before announced_at". This is
	// the main anti-sybil control alongside min_account_age_days: a farm of
	// accounts created for the event has no history before it.
	//
	// Runs whenever we hold the applicant's own token, which in practice is
	// always - applying already requires a linked GitHub account. It fails
	// open only on a genuine API error (rate limit, outage), never on a
	// missing-credential technicality, because a check that silently stops
	// enforcing is worse than one that visibly errors.
	if env.requirePreAnnounceCommit {
		if ac.SearchToken == "" {
			// Not a normal state: the apply path resolves this before
			// calling. Log-worthy for the caller rather than a silent pass.
			return GateResult{}, fmt.Errorf(
				"hackathon.CheckGates: require_pre_announcement_commit is on but no applicant token was supplied for %s", ac.GitHubLogin)
		}
		// Checked against the applicant's own public activity rather than
		// this repo: requiring prior commits *to the hackathon repo* would
		// make every issue insider-only, the exact opposite of the intent.
		hadPrior, err := gh.UserHasPublicActivityBefore(ctx, ac.SearchToken, ac.GitHubLogin, env.announcedAt, env.minPreAnnounceCommits)
		switch {
		case err != nil:
			// Genuine API failure - fail open, and say so loudly enough
			// that a persistently broken gate is visible in logs rather
			// than quietly waving everyone through forever.
			slog.Warn("hackathon: pre-event activity check failed, allowing the application",
				"login", ac.GitHubLogin, "error", err)
		case !hadPrior:
			return gateFail("pre_event_activity", "Your account needs public commit history from before this event was announced."), nil
		}
	}

	return gatePass(), nil
}
