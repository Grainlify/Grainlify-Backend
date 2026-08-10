package hackathon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AI-specs.md §7. Four criteria decide a maintainer's share; the fifth -
// "repo still active 60 days after the event" - is not scored here because it
// cannot be known at settle time. It is what the holdback release is
// conditional on, which is the only thing that makes the holdback do any work.

// Criterion keys. §7's table marks these safe precisely because a maintainer
// cannot move them alone: issues created, PRs merged and stars are all
// excluded for the opposite reason.
const (
	CriterionFirstTimeContributors = "first_time_contributors"
	CriterionTimeToFirstReview     = "median_time_to_first_review_hours"
	CriterionRepoPredatesEvent     = "repo_had_commits_before_announced_at"
	CriterionIssueClarity          = "issue_clarity_rating"
)

// defaultCriteriaWeights is used when maintainer_criteria_weights is unset.
// Deliberately even: nothing yet justifies asserting one of these matters
// more than another, and an invented weighting would be indistinguishable
// from a measured one once it is in production.
var defaultCriteriaWeights = map[string]float64{
	CriterionFirstTimeContributors: 0.25,
	CriterionTimeToFirstReview:     0.25,
	CriterionRepoPredatesEvent:     0.25,
	CriterionIssueClarity:          0.25,
}

// Criterion is one scored input, snapshotted.
type Criterion struct {
	Key string `json:"key"`
	// Included is false when the criterion was dropped - too little data to
	// be meaningful. Its weight is then redistributed across the rest rather
	// than scored as zero, because "we could not measure this" and "this repo
	// did badly" are different claims and only one of them is true.
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
	// Raw is the measured value in its own units, kept so a human reading a
	// snapshot months later can sanity-check the normalisation.
	Raw        *float64 `json:"raw"`
	Normalized float64  `json:"normalized"`
	// Weight after renormalisation - what actually multiplied Normalized.
	Weight float64 `json:"weight"`
}

// MaintainerScore is one repo's score with every input that produced it.
type MaintainerScore struct {
	ProjectID  uuid.UUID   `json:"project_id"`
	OrgLogin   string      `json:"org_login"`
	Score      float64     `json:"score"`
	Criteria   []Criterion `json:"criteria"`
	ComputedAt time.Time   `json:"computed_at"`
}

// criteriaWeights resolves the configured weights, falling back to the
// defaults for any key the config does not mention.
func criteriaWeights(cfg map[string]string) map[string]float64 {
	weights := map[string]float64{}
	for k, v := range defaultCriteriaWeights {
		weights[k] = v
	}
	raw := cfg["maintainer_criteria_weights"]
	if raw == "" || raw == "{}" {
		return weights
	}
	var parsed map[string]float64
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return weights
	}
	for k, v := range parsed {
		if _, known := weights[k]; known && v >= 0 {
			weights[k] = v
		}
	}
	return weights
}

// scoreFrom applies renormalisation and returns the weighted score.
//
// Dropping a criterion redistributes its weight proportionally across those
// that remain, so a repo with too few clarity ratings is scored on what could
// actually be measured rather than being handed a zero for the missing one. A
// zero would be a factual claim the data does not support - and it is the
// same argument as the anonymity floor, applied to money instead of privacy.
func scoreFrom(criteria []Criterion) (float64, []Criterion) {
	var totalIncluded float64
	for _, c := range criteria {
		if c.Included {
			totalIncluded += c.Weight
		}
	}
	if totalIncluded <= 0 {
		for i := range criteria {
			criteria[i].Weight = 0
		}
		return 0, criteria
	}

	var score float64
	for i := range criteria {
		if !criteria[i].Included {
			criteria[i].Weight = 0
			continue
		}
		criteria[i].Weight = criteria[i].Weight / totalIncluded
		score += criteria[i].Normalized * criteria[i].Weight
	}
	return score, criteria
}

func floatPtr(v float64) *float64 { return &v }

// clampUnit keeps a normalised value inside [0,1].
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ComputeMaintainerScore builds one repo's score from stored data.
//
// reviewMedianHours is passed in rather than fetched here because it is
// sampled from the GitHub API (see docs/TESTING-DEBT.md) - keeping the call
// outside makes this function a pure-ish computation over stored rows, so the
// scoring rules are testable without a network.
//
// A nil reviewMedianHours drops that criterion rather than scoring it zero,
// for the same reason as the clarity floor.
func ComputeMaintainerScore(
	ctx context.Context,
	pool db.DBPool,
	hackathonID, projectID uuid.UUID,
	reviewMedianHours *float64,
	cfg map[string]string,
) (MaintainerScore, error) {
	out := MaintainerScore{ProjectID: projectID, ComputedAt: time.Now()}

	var orgLogin string
	var announcedAt *time.Time
	if err := pool.QueryRow(ctx, `
SELECT SPLIT_PART(p.github_full_name, '/', 1), h.announced_at
FROM projects p CROSS JOIN hackathons h
WHERE p.id = $1 AND h.id = $2
`, projectID, hackathonID).Scan(&orgLogin, &announcedAt); err != nil {
		return out, fmt.Errorf("hackathon.ComputeMaintainerScore: load context: %w", err)
	}
	out.OrgLogin = orgLogin

	weights := criteriaWeights(cfg)
	var criteria []Criterion

	// 1. Distinct first-time contributors to this repo during the event.
	// "First-time" means their earliest recorded contribution to this repo
	// falls inside the event, which is what makes it a growth signal rather
	// than a measure of existing traffic.
	var firstTimers int
	if err := pool.QueryRow(ctx, `
WITH firsts AS (
  SELECT LOWER(author_login) AS login, MIN(created_at_github) AS first_at
  FROM (
    SELECT author_login, created_at_github FROM github_pull_requests
    WHERE project_id = $1 AND author_login IS NOT NULL AND author_login <> ''
    UNION ALL
    SELECT author_login, created_at_github FROM github_issues
    WHERE project_id = $1 AND author_login IS NOT NULL AND author_login <> ''
  ) x
  WHERE created_at_github IS NOT NULL
  GROUP BY 1
)
SELECT count(*)::int FROM firsts
WHERE $2::timestamptz IS NULL OR first_at >= $2::timestamptz
`, projectID, announcedAt).Scan(&firstTimers); err != nil {
		return out, fmt.Errorf("hackathon.ComputeMaintainerScore: first-timers: %w", err)
	}
	// Normalised against a soft target of 10 newcomers; above that is full
	// marks rather than unbounded advantage, so one very large repo cannot
	// dominate the pool on scale alone.
	criteria = append(criteria, Criterion{
		Key: CriterionFirstTimeContributors, Included: true,
		Raw: floatPtr(float64(firstTimers)), Normalized: clampUnit(float64(firstTimers) / 10.0),
		Weight: weights[CriterionFirstTimeContributors],
	})

	// 2. Median time to first review. Faster is better; 72h or worse scores
	// zero, immediate scores one.
	c2 := Criterion{Key: CriterionTimeToFirstReview, Weight: weights[CriterionTimeToFirstReview]}
	if reviewMedianHours == nil {
		c2.Reason = "No review data available for this repo, so the criterion is dropped and its weight redistributed."
	} else {
		c2.Included = true
		c2.Raw = reviewMedianHours
		c2.Normalized = clampUnit(1.0 - (*reviewMedianHours / 72.0))
	}
	criteria = append(criteria, c2)

	// 3. Repo predates the event. Binary, and the cheapest signal that the
	// repo was not spun up to farm it.
	var predates bool
	if announcedAt != nil {
		if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM github_pull_requests WHERE project_id = $1 AND created_at_github < $2
  UNION ALL
  SELECT 1 FROM github_issues WHERE project_id = $1 AND created_at_github < $2
)
`, projectID, announcedAt).Scan(&predates); err != nil {
			return out, fmt.Errorf("hackathon.ComputeMaintainerScore: predates: %w", err)
		}
	}
	norm := 0.0
	if predates {
		norm = 1.0
	}
	criteria = append(criteria, Criterion{
		Key: CriterionRepoPredatesEvent, Included: announcedAt != nil,
		Raw: floatPtr(norm), Normalized: norm, Weight: weights[CriterionRepoPredatesEvent],
		Reason: func() string {
			if announcedAt == nil {
				return "The hackathon has no announced_at, so there is no boundary to test the repo against."
			}
			return ""
		}(),
	})

	// 4. Issue clarity, subject to a minimum sample. One rating deciding a
	// quarter of someone's payout is noise deciding money.
	minRatings := atoiOr(cfg["maintainer_clarity_min_ratings"], 3)
	if minRatings < 1 {
		minRatings = 1
	}
	count, mean, err := ClarityScore(ctx, pool, hackathonID, projectID)
	if err != nil {
		return out, err
	}
	c4 := Criterion{Key: CriterionIssueClarity, Weight: weights[CriterionIssueClarity]}
	if count < minRatings {
		c4.Reason = fmt.Sprintf("Only %d clarity rating(s); %d are needed before this counts. Dropped and its weight redistributed rather than scored as zero.", count, minRatings)
	} else {
		c4.Included = true
		c4.Raw = floatPtr(mean)
		// 1..5 stars onto 0..1.
		c4.Normalized = clampUnit((mean - 1.0) / 4.0)
	}
	criteria = append(criteria, c4)

	sort.Slice(criteria, func(i, j int) bool { return criteria[i].Key < criteria[j].Key })
	out.Score, out.Criteria = scoreFrom(criteria)
	return out, nil
}

// MaintainerPayout is one repo's allocation from the maintainer pool.
type MaintainerPayout struct {
	ProjectID       uuid.UUID `json:"project_id"`
	OrgLogin        string    `json:"org_login"`
	Score           float64   `json:"score"`
	GrossAmount     float64   `json:"gross_amount"`
	HoldbackPct     int       `json:"holdback_pct"`
	HoldbackAmount  float64   `json:"holdback_amount"`
	ImmediateAmount float64   `json:"immediate_amount"`
	HoldbackDueAt   time.Time `json:"holdback_due_at"`
}

// AllocateMaintainerPool divides maintainerPool across scored repos in
// proportion to score, then splits each share into an immediate part and a
// holdback.
//
// maintainerPool is the *only* money that enters here. It is read from
// hackathons.maintainer_prize_pool by the caller and never from
// contributor_prize_pool - §7's opening requirement, and the reason the two
// are separate columns rather than one pot with a label.
func AllocateMaintainerPool(scores []MaintainerScore, maintainerPool float64, cfg map[string]string, settledAt time.Time) []MaintainerPayout {
	holdbackPct := atoiOr(cfg["maintainer_holdback_pct"], 30)
	if holdbackPct < 0 {
		holdbackPct = 0
	}
	if holdbackPct > 100 {
		holdbackPct = 100
	}
	holdbackDays := atoiOr(cfg["maintainer_holdback_days"], 90)
	if holdbackDays < 0 {
		holdbackDays = 0
	}
	due := settledAt.AddDate(0, 0, holdbackDays)

	var totalScore float64
	for _, s := range scores {
		if s.Score > 0 {
			totalScore += s.Score
		}
	}

	out := make([]MaintainerPayout, 0, len(scores))
	for _, s := range scores {
		p := MaintainerPayout{
			ProjectID: s.ProjectID, OrgLogin: s.OrgLogin, Score: s.Score,
			HoldbackPct: holdbackPct, HoldbackDueAt: due,
		}
		if totalScore > 0 && s.Score > 0 && maintainerPool > 0 {
			p.GrossAmount = round2(maintainerPool * (s.Score / totalScore))
			p.HoldbackAmount = round2(p.GrossAmount * float64(holdbackPct) / 100.0)
			p.ImmediateAmount = round2(p.GrossAmount - p.HoldbackAmount)
		}
		out = append(out, p)
	}
	return out
}

// RepoActivity is the evidence a holdback release is decided on.
type RepoActivity struct {
	WindowDays    int       `json:"window_days"`
	Since         time.Time `json:"since"`
	Commits       int       `json:"commits"`
	MergedPRs     int       `json:"merged_prs"`
	IssueActivity int       `json:"issue_activity"`
	// CommitsMeasured is false when the commit count could not be fetched.
	// Recorded because "no commits" and "we could not look" must not collapse
	// into the same decision when money is being withheld.
	CommitsMeasured bool `json:"commits_measured"`
}

// HoldbackDecision is what to do with a held-back amount.
type HoldbackDecision struct {
	Status      string       `json:"status"`
	Released    float64      `json:"released_amount"`
	Withheld    float64      `json:"withheld_amount"`
	Destination string       `json:"withheld_destination,omitempty"`
	Reason      string       `json:"reason"`
	Activity    RepoActivity `json:"activity"`
}

// DecideHoldback applies §7's fifth criterion - "repo still active 60 days
// after the event" - to a held-back amount.
//
// This is the whole point of the holdback. §7: "A maintainer farming an event
// is gone the next day. One genuinely growing a project is still there. This
// single mechanism does more anti-farming work than any classifier in this
// document." A holdback that releases on a timer alone tests none of that -
// the farmer waits out the clock and is paid in full, three months late.
//
// Three outcomes rather than two, because "one commit" and "nothing at all"
// are genuinely different and a binary would have to lump one of them in with
// the wrong neighbour.
//
// Withheld money goes to maintainer_withheld_destination, published in
// advance. Deciding afterwards where forfeited money lands is how a program
// ends up looking like it kept it.
func DecideHoldback(payout MaintainerPayout, activity RepoActivity, cfg map[string]string) HoldbackDecision {
	fullCommits := atoiOr(cfg["maintainer_activity_full_commits"], 5)
	fullPRs := atoiOr(cfg["maintainer_activity_full_prs"], 2)
	partialPct := atoiOr(cfg["maintainer_partial_release_pct"], 50)
	if partialPct < 0 {
		partialPct = 0
	}
	if partialPct > 100 {
		partialPct = 100
	}
	destination := cfg["maintainer_withheld_destination"]
	if destination == "" {
		destination = "next_event_pool"
	}

	d := HoldbackDecision{Activity: activity, Destination: destination}

	sustained := activity.Commits >= fullCommits || activity.MergedPRs >= fullPRs
	any := activity.Commits > 0 || activity.MergedPRs > 0 || activity.IssueActivity > 0

	switch {
	case sustained:
		d.Status = "released"
		d.Released = payout.HoldbackAmount
		d.Destination = ""
		d.Reason = fmt.Sprintf("Repo stayed active in the %d days after the event: %d commit(s) and %d merged PR(s).",
			activity.WindowDays, activity.Commits, activity.MergedPRs)
	case any:
		d.Status = "partially_released"
		d.Released = round2(payout.HoldbackAmount * float64(partialPct) / 100.0)
		d.Withheld = round2(payout.HoldbackAmount - d.Released)
		d.Reason = fmt.Sprintf("Some activity in the %d days after the event (%d commit(s), %d merged PR(s), %d issue event(s)) but below the bar of %d commits or %d merged PRs, so %d%% of the holdback is released.",
			activity.WindowDays, activity.Commits, activity.MergedPRs, activity.IssueActivity, fullCommits, fullPRs, partialPct)
	case !activity.CommitsMeasured:
		// Failing to look is not evidence of absence. Withholding someone's
		// money because a GitHub call failed would be a bug that reads as a
		// judgement, so this stays pending and is retried.
		d.Status = "pending"
		d.Reason = "Repo activity could not be measured, so the holdback stays pending rather than being withheld on missing evidence."
	default:
		d.Status = "withheld"
		d.Withheld = payout.HoldbackAmount
		d.Reason = fmt.Sprintf("No commits, merged PRs or issue activity in the %d days after the event.", activity.WindowDays)
	}
	return d
}
