package hackathon

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// JudgingStats summarises an event's judging, with the cross-check
// disagreement rate as the headline number.
//
// AI-specs.md §5.6 predicts 5-15% escalation. That figure is worth treating
// as a diagnostic rather than a capacity plan: if the real rate comes back
// far higher, the problem is very unlikely to be the models. Two competent
// judges disagreeing 40% of the time means the *bucket definitions* are
// ambiguous - most often the accepted/substantial line, which §8 warns is
// exactly the boundary hand-labelling always turns out to have been fuzzy
// about. The fix in that case is the definitions and the calibration set,
// not a bigger review queue or a better model.
type JudgingStats struct {
	Total int `json:"total"`
	// BothJudged is the denominator for the disagreement rate: verdicts
	// where a judge and a cross-check both returned a bucket. A missing
	// cross-check is not a disagreement.
	BothJudged    int `json:"both_judged"`
	Disagreements int `json:"disagreements"`
	// DisagreementRate is a percentage, or nil when nothing has been
	// double-judged yet - a rate over zero samples is not zero, it is
	// unknown, and showing 0% would read as perfect agreement.
	DisagreementRate *float64 `json:"disagreement_rate"`
	// ExpectedRangeLow/High are §5.6's prediction, carried alongside the
	// measurement so the number is readable without the spec to hand.
	ExpectedRangeLow  float64 `json:"expected_range_low"`
	ExpectedRangeHigh float64 `json:"expected_range_high"`

	NeedsReview      int `json:"needs_review"`
	Overridden       int `json:"overridden"`
	PrefilterOut     int `json:"prefiltered_out"`
	CrossChecked     int `json:"cross_checked"`
	InjectionFlagged int `json:"injection_flagged"`
	// DisagreementByPair counts each judge->cross-check bucket pairing, so
	// a high rate can be read for *where* the ambiguity is rather than just
	// how much there is.
	DisagreementByPair map[string]int `json:"disagreement_by_pair"`
}

// ComputeJudgingStats reads an event's verdicts and derives §5.6's metric.
func ComputeJudgingStats(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) (*JudgingStats, error) {
	s := &JudgingStats{
		ExpectedRangeLow:   5,
		ExpectedRangeHigh:  15,
		DisagreementByPair: map[string]int{},
	}

	err := pool.QueryRow(ctx, `
SELECT
  count(*),
  count(*) FILTER (WHERE judge_bucket IS NOT NULL AND cross_check_bucket IS NOT NULL),
  count(*) FILTER (WHERE judge_bucket IS NOT NULL AND cross_check_bucket IS NOT NULL
                     AND judge_bucket <> cross_check_bucket),
  count(*) FILTER (WHERE needs_human_review),
  count(*) FILTER (WHERE overridden_at IS NOT NULL),
  count(*) FILTER (WHERE prefilter_status = 'rejected'),
  count(*) FILTER (WHERE cross_check_bucket IS NOT NULL),
  count(*) FILTER (WHERE judge_payload -> 'concerns' ? 'instruction_injection_attempt')
FROM hackathon_verdicts
WHERE hackathon_id = $1
`, hackathonID).Scan(&s.Total, &s.BothJudged, &s.Disagreements, &s.NeedsReview,
		&s.Overridden, &s.PrefilterOut, &s.CrossChecked, &s.InjectionFlagged)
	if err != nil {
		return nil, fmt.Errorf("hackathon.ComputeJudgingStats: %w", err)
	}

	if s.BothJudged > 0 {
		rate := 100 * float64(s.Disagreements) / float64(s.BothJudged)
		s.DisagreementRate = &rate
	}

	rows, err := pool.Query(ctx, `
SELECT judge_bucket || ' -> ' || cross_check_bucket, count(*)
FROM hackathon_verdicts
WHERE hackathon_id = $1
  AND judge_bucket IS NOT NULL AND cross_check_bucket IS NOT NULL
  AND judge_bucket <> cross_check_bucket
GROUP BY 1
ORDER BY 2 DESC
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.ComputeJudgingStats: pairs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pair string
		var n int
		if err := rows.Scan(&pair, &n); err != nil {
			return nil, err
		}
		s.DisagreementByPair[pair] = n
	}
	return s, rows.Err()
}
