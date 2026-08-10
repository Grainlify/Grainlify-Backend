package hackathon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func statsFxVerdict(t *testing.T, pool db.DBPool, hackathonID, projectID uuid.UUID, pr int, judge, cross string, concerns []string) {
	t.Helper()
	var judgePayload []byte
	if concerns == nil {
		concerns = []string{}
	}
	judgePayload, _ = json.Marshal(map[string]any{"concerns": concerns})

	var jb, cb *string
	if judge != "" {
		jb = &judge
	}
	if cross != "" {
		cb = &cross
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, pr_number, github_login, prefilter_status,
   judge_bucket, cross_check_bucket, judge_payload)
VALUES ($1,$2,$3,'octocat','passed',$4,$5,$6)
`, hackathonID, projectID, pr, jb, cb, judgePayload); err != nil {
		t.Fatalf("statsFxVerdict: %v", err)
	}
}

func TestComputeJudgingStats_DisagreementRate(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	// 8 double-judged, 2 disagreeing -> 25%.
	for i := 0; i < 6; i++ {
		statsFxVerdict(t, pool, hackathonID, projectID, 600+i, "accepted", "accepted", nil)
	}
	statsFxVerdict(t, pool, hackathonID, projectID, 610, "substantial", "accepted", nil)
	statsFxVerdict(t, pool, hackathonID, projectID, 611, "exceptional", "substantial", nil)
	// Judge only - an absent cross-check is not a disagreement and must not
	// inflate the denominator.
	statsFxVerdict(t, pool, hackathonID, projectID, 620, "accepted", "", nil)
	statsFxVerdict(t, pool, hackathonID, projectID, 621, "accepted", "", nil)

	s, err := ComputeJudgingStats(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("ComputeJudgingStats: %v", err)
	}
	if s.Total != 10 {
		t.Errorf("Total = %d, want 10", s.Total)
	}
	if s.BothJudged != 8 {
		t.Errorf("BothJudged = %d, want 8 - a missing cross-check is not a sample", s.BothJudged)
	}
	if s.Disagreements != 2 {
		t.Errorf("Disagreements = %d, want 2", s.Disagreements)
	}
	if s.DisagreementRate == nil || *s.DisagreementRate != 25 {
		t.Errorf("DisagreementRate = %v, want 25", s.DisagreementRate)
	}
	// The pairing is what makes a high rate actionable: it says *where* the
	// bucket definitions are ambiguous.
	if s.DisagreementByPair["substantial -> accepted"] != 1 {
		t.Errorf("pairs = %v, want the substantial->accepted pairing counted", s.DisagreementByPair)
	}
	if s.ExpectedRangeLow != 5 || s.ExpectedRangeHigh != 15 {
		t.Errorf("expected range = %v-%v, want §5.6's 5-15", s.ExpectedRangeLow, s.ExpectedRangeHigh)
	}
}

// A rate over zero samples is unknown, not zero. Rendering 0% would read as
// perfect agreement when nothing has been double-judged at all.
func TestComputeJudgingStats_NoSamplesMeansUnknownNotZero(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	statsFxVerdict(t, pool, hackathonID, projectID, 630, "accepted", "", nil)

	s, err := ComputeJudgingStats(context.Background(), pool, hackathonID)
	if err != nil {
		t.Fatalf("ComputeJudgingStats: %v", err)
	}
	if s.DisagreementRate != nil {
		t.Errorf("DisagreementRate = %v, want nil with no double-judged verdicts", *s.DisagreementRate)
	}
}

func TestComputeJudgingStats_CountsInjectionFlags(t *testing.T) {
	d := dbtest.DB(t)
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	statsFxVerdict(t, pool, hackathonID, projectID, 640, "accepted", "accepted", []string{ConcernInjection})
	statsFxVerdict(t, pool, hackathonID, projectID, 641, "accepted", "accepted", []string{"diff_truncated"})

	s, err := ComputeJudgingStats(context.Background(), pool, hackathonID)
	if err != nil {
		t.Fatalf("ComputeJudgingStats: %v", err)
	}
	if s.InjectionFlagged != 1 {
		t.Errorf("InjectionFlagged = %d, want 1", s.InjectionFlagged)
	}
}
