package hackathon

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// signalPagesCap bounds the paginated GitHub calls below - these back an
// admin-facing display signal, not a billing or eligibility decision, so an
// approximate-but-fast count is preferable to an exhaustive one.
const signalPagesCap = 3

// reviewSampleSize is how many recent PRs MedianTimeToFirstReviewHours
// samples from.
const reviewSampleSize = 20

// Signal wraps one auto-collected value with whether it was actually
// computed - a GitHub call failing for one signal must never fail the whole
// response (AI-specs.md §2.1's signals are informational, not gating).
type Signal struct {
	Computed bool   `json:"computed"`
	Value    any    `json:"value,omitempty"`
	Note     string `json:"note,omitempty"` // set when Computed is false, or to explain a capped/partial value
}

// PriorApplication is one other hackathon this project has applied to,
// shown as part of "has this org participated in a prior GrainHack, and its
// outcome" (§2.1) - the "outcome" is only ever accepted/rejected/pending in
// this slice, since judging/results don't exist yet.
type PriorApplication struct {
	HackathonName string `json:"hackathon_name"`
	Status        string `json:"status"`
}

// Signals is the full §2.1 auto-collected-signals set for one application,
// cached into hackathon_project_applications.signals.
type Signals struct {
	RepoCreatedAt                Signal `json:"repo_created_at"`
	HadCommitsBeforeAnnounced    Signal `json:"had_commits_before_announced"`
	CommitActivity90d            Signal `json:"commit_activity_90d"`
	DistinctContributors         Signal `json:"distinct_contributors"`
	PriorGrainHackParticipation  Signal `json:"prior_grainhack_participation"`
	MedianTimeToFirstReviewHours Signal `json:"median_time_to_first_review_hours"`
	// Depends on the collusion machinery (AI-specs.md §5.3), which is a
	// future slice - always unavailable here, with an explicit caveat
	// rather than silently omitted, so the spec's field list is honored.
	PriorFlaggedAssociations Signal `json:"prior_flagged_associations"`
}

// Compute gathers every §2.1 signal for one hackathon_project_applications
// row. Each field is attempted independently; a failure on one never
// prevents the others from being returned.
func Compute(ctx context.Context, cfg config.Config, pool db.DBPool, gh *github.Client, applicationID uuid.UUID) (Signals, error) {
	var s Signals

	var projectID uuid.UUID
	var fullName, installationID string
	var announcedAt *time.Time
	err := pool.QueryRow(ctx, `
SELECT p.id, p.github_full_name, COALESCE(p.github_app_installation_id, ''), h.announced_at
FROM hackathon_project_applications hpa
JOIN projects p ON p.id = hpa.project_id
JOIN hackathons h ON h.id = hpa.hackathon_id
WHERE hpa.id = $1
`, applicationID).Scan(&projectID, &fullName, &installationID, &announcedAt)
	if err != nil {
		return s, fmt.Errorf("hackathon.Compute: load application: %w", err)
	}

	// Prior GrainHack participation is derived from our own DB, not GitHub -
	// compute it regardless of whether a GitHub token is available below.
	s.PriorGrainHackParticipation = computePriorParticipation(ctx, pool, projectID, applicationID)

	s.PriorFlaggedAssociations = Signal{Computed: false, Note: "Not available until collusion-signal tracking (AI-specs.md §5.3) is built."}

	if installationID == "" {
		note := "Project has no GitHub App installation; GitHub-derived signals are unavailable."
		s.RepoCreatedAt = Signal{Computed: false, Note: note}
		s.HadCommitsBeforeAnnounced = Signal{Computed: false, Note: note}
		s.CommitActivity90d = Signal{Computed: false, Note: note}
		s.DistinctContributors = Signal{Computed: false, Note: note}
		s.MedianTimeToFirstReviewHours = Signal{Computed: false, Note: note}
		return s, nil
	}

	appClient, err := github.NewGitHubAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKey)
	if err != nil {
		note := "Could not build a GitHub App client: " + err.Error()
		s.RepoCreatedAt = Signal{Computed: false, Note: note}
		s.HadCommitsBeforeAnnounced = Signal{Computed: false, Note: note}
		s.CommitActivity90d = Signal{Computed: false, Note: note}
		s.DistinctContributors = Signal{Computed: false, Note: note}
		s.MedianTimeToFirstReviewHours = Signal{Computed: false, Note: note}
		return s, nil
	}
	token, err := appClient.GetInstallationToken(ctx, installationID)
	if err != nil {
		note := "Could not fetch an installation token: " + err.Error()
		s.RepoCreatedAt = Signal{Computed: false, Note: note}
		s.HadCommitsBeforeAnnounced = Signal{Computed: false, Note: note}
		s.CommitActivity90d = Signal{Computed: false, Note: note}
		s.DistinctContributors = Signal{Computed: false, Note: note}
		s.MedianTimeToFirstReviewHours = Signal{Computed: false, Note: note}
		return s, nil
	}

	if repo, err := gh.GetRepo(ctx, token, fullName); err != nil {
		s.RepoCreatedAt = Signal{Computed: false, Note: err.Error()}
	} else {
		s.RepoCreatedAt = Signal{Computed: true, Value: repo.CreatedAt}
	}

	if announcedAt != nil {
		if had, err := gh.HasCommitBefore(ctx, token, fullName, *announcedAt); err != nil {
			s.HadCommitsBeforeAnnounced = Signal{Computed: false, Note: err.Error()}
		} else {
			s.HadCommitsBeforeAnnounced = Signal{Computed: true, Value: had}
		}
	} else {
		s.HadCommitsBeforeAnnounced = Signal{Computed: false, Note: "Hackathon has no announced_at date set yet."}
	}

	since := time.Now().AddDate(0, 0, -90)
	if count, capped, err := gh.CountCommitsSince(ctx, token, fullName, since, signalPagesCap); err != nil {
		s.CommitActivity90d = Signal{Computed: false, Note: err.Error()}
	} else {
		sig := Signal{Computed: true, Value: count}
		if capped {
			sig.Note = fmt.Sprintf("Capped at %d pages - actual count may be higher.", signalPagesCap)
		}
		s.CommitActivity90d = sig
	}

	if count, capped, err := gh.CountContributors(ctx, token, fullName, signalPagesCap); err != nil {
		s.DistinctContributors = Signal{Computed: false, Note: err.Error()}
	} else {
		sig := Signal{Computed: true, Value: count}
		if capped {
			sig.Note = fmt.Sprintf("Capped at %d pages - actual count may be higher.", signalPagesCap)
		}
		s.DistinctContributors = sig
	}

	if median, err := gh.MedianTimeToFirstReviewHours(ctx, token, fullName, reviewSampleSize); err != nil {
		s.MedianTimeToFirstReviewHours = Signal{Computed: false, Note: err.Error()}
	} else if median == nil {
		s.MedianTimeToFirstReviewHours = Signal{Computed: false, Note: "No reviewed PRs found in the recent sample."}
	} else {
		s.MedianTimeToFirstReviewHours = Signal{Computed: true, Value: *median}
	}

	return s, nil
}

func computePriorParticipation(ctx context.Context, pool db.DBPool, projectID, currentApplicationID uuid.UUID) Signal {
	rows, err := pool.Query(ctx, `
SELECT h.name, hpa.status
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
WHERE hpa.project_id = $1 AND hpa.id != $2
ORDER BY hpa.created_at DESC
`, projectID, currentApplicationID)
	if err != nil {
		return Signal{Computed: false, Note: err.Error()}
	}
	defer rows.Close()

	var prior []PriorApplication
	for rows.Next() {
		var p PriorApplication
		if err := rows.Scan(&p.HackathonName, &p.Status); err != nil {
			return Signal{Computed: false, Note: err.Error()}
		}
		prior = append(prior, p)
	}
	if err := rows.Err(); err != nil {
		return Signal{Computed: false, Note: err.Error()}
	}
	return Signal{Computed: true, Value: prior}
}
