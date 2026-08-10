package hackathon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AI-specs.md §7: "Contributor rating of issue clarity, collected at PR
// submission (before payout is known)" - the criterion the spec calls the
// best proxy for genuine growth intent, and the only one of the four that a
// maintainer cannot manufacture alone.

// ClarityMinRatingsToShow is how many ratings a repo needs before its
// aggregate is shown to the maintainer at all.
//
// Withholding the aggregate until the event closes stops a maintainer
// inferring who rated what from arrival timing. That is necessary but not
// sufficient: with a single rating the "aggregate" *is* that person's rating,
// and with two a maintainer who recognises one can derive the other. Three is
// the smallest floor that leaves no single contributor's answer readable, and
// these people still need PRs merged by the person they are rating.
const ClarityMinRatingsToShow = 3

var (
	ErrClarityNotYourAssignment = errors.New("that assignment belongs to someone else")
	ErrClarityWindowClosed      = errors.New("issue clarity ratings close when results are published")
)

// SubmitClarityRating records a contributor's rating of how clear an issue
// was. Optional everywhere: nothing calls this on the path to submitting a
// PR, and skipping simply leaves no row.
//
// Re-rating before results are published overwrites the previous answer -
// someone correcting a misclick is not two data points.
func SubmitClarityRating(
	ctx context.Context,
	pool db.DBPool,
	assignmentID, userID uuid.UUID,
	rating int,
	comment string,
) error {
	if rating < 1 || rating > 5 {
		return fmt.Errorf("hackathon.SubmitClarityRating: rating must be between 1 and 5")
	}

	var (
		hackathonID uuid.UUID
		projectID   uuid.UUID
		issueID     *uuid.UUID
		issueNumber int
		owner       uuid.UUID
	)
	err := pool.QueryRow(ctx, `
SELECT hackathon_id, project_id, hackathon_issue_id, issue_number, user_id
FROM hackathon_assignments WHERE id = $1
`, assignmentID).Scan(&hackathonID, &projectID, &issueID, &issueNumber, &owner)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("hackathon.SubmitClarityRating: assignment %s not found", assignmentID)
		}
		return fmt.Errorf("hackathon.SubmitClarityRating: load assignment: %w", err)
	}
	if owner != userID {
		return ErrClarityNotYourAssignment
	}

	// Enforced here as well as excluded at scoring time. Refusing the write
	// gives the contributor an honest answer now; the scoring-time exclusion
	// is what protects the number if a row ever lands late anyway.
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return err
	}
	if h.ResultsPublishedAt != nil {
		return ErrClarityWindowClosed
	}

	_, err = pool.Exec(ctx, `
INSERT INTO hackathon_issue_clarity_ratings
  (hackathon_id, assignment_id, project_id, hackathon_issue_id, issue_number, user_id, rating, comment)
VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))
ON CONFLICT (assignment_id, user_id) DO UPDATE SET
  rating = EXCLUDED.rating,
  comment = EXCLUDED.comment,
  submitted_at = now()
`, hackathonID, assignmentID, projectID, issueID, issueNumber, userID, rating, strings.TrimSpace(comment))
	if err != nil {
		return fmt.Errorf("hackathon.SubmitClarityRating: %w", err)
	}
	return nil
}

// ClarityAggregate is what a maintainer is allowed to see: a mean and a
// count, never an individual rating, never a comment attributed to anyone.
type ClarityAggregate struct {
	ProjectID uuid.UUID `json:"project_id"`
	// Visible is false when the event has not closed yet, or when too few
	// ratings exist to aggregate without exposing an individual. Reason says
	// which, so the UI can explain rather than render an empty panel.
	Visible bool    `json:"visible"`
	Reason  string  `json:"reason,omitempty"`
	Count   int     `json:"count,omitempty"`
	Mean    float64 `json:"mean,omitempty"`
}

// ClarityForMaintainer returns the aggregate a maintainer may see for one
// project.
//
// Two gates, both required:
//
//  1. The event has closed. Live ratings are a retaliation channel: a
//     maintainer watching them arrive can infer who rated what from timing,
//     and the people rating them still need PRs merged.
//  2. At least ClarityMinRatingsToShow ratings exist, so no individual
//     answer is readable from the mean.
//
// Only ratings submitted before results were published are counted, matching
// the scoring rule exactly - a maintainer and an admin must not be looking at
// different numbers.
func ClarityForMaintainer(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID) (ClarityAggregate, error) {
	agg := ClarityAggregate{ProjectID: projectID}

	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return agg, err
	}
	if phaseIndex(h.Phase) < phaseIndex("closed") {
		agg.Reason = "Ratings are shown once the event closes. Seeing them arrive live would make it possible to work out who said what."
		return agg, nil
	}

	count, mean, err := clarityStats(ctx, pool, hackathonID, projectID, h.ResultsPublishedAt)
	if err != nil {
		return agg, err
	}
	if count < ClarityMinRatingsToShow {
		agg.Reason = fmt.Sprintf("Not enough ratings to show an average without revealing an individual answer (%d of %d needed).", count, ClarityMinRatingsToShow)
		return agg, nil
	}

	agg.Visible = true
	agg.Count = count
	agg.Mean = mean
	return agg, nil
}

// ClarityScore is the scoring-side view: the same numbers with no visibility
// gate, for the admin and the maintainer-pool computation.
func ClarityScore(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID) (count int, mean float64, err error) {
	h, err := loadHackathon(ctx, pool, hackathonID)
	if err != nil {
		return 0, 0, err
	}
	return clarityStats(ctx, pool, hackathonID, projectID, h.ResultsPublishedAt)
}

// clarityStats counts only ratings submitted strictly before results were
// published.
//
// The exclusion is asserted here rather than trusted from the write path. A
// row can arrive late through a replayed request, a backfill, or a future
// code path that forgets the rule; the scoring number is what decides money,
// so it re-checks rather than assuming.
func clarityStats(
	ctx context.Context,
	pool db.DBPool,
	hackathonID, projectID uuid.UUID,
	resultsPublishedAt *time.Time,
) (int, float64, error) {
	// A nil cutoff means results are not published, so nothing can be late.
	cutoff := time.Now().Add(100 * 365 * 24 * time.Hour)
	if resultsPublishedAt != nil {
		cutoff = *resultsPublishedAt
	}

	var count int
	var mean *float64
	err := pool.QueryRow(ctx, `
SELECT COUNT(*)::int, AVG(rating)::float8
FROM hackathon_issue_clarity_ratings
WHERE hackathon_id = $1 AND project_id = $2 AND submitted_at < $3
`, hackathonID, projectID, cutoff).Scan(&count, &mean)
	if err != nil {
		return 0, 0, fmt.Errorf("hackathon.clarityStats: %w", err)
	}
	if mean == nil {
		return count, 0, nil
	}
	return count, *mean, nil
}
