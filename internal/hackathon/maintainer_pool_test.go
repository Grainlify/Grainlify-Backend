package hackathon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// A criterion with too little data is dropped and its weight redistributed,
// never scored as zero. "We could not measure this" and "this repo did badly"
// are different claims, and only one of them is supported.
func TestComputeMaintainerScore_DropsThinCriteriaAndRenormalises(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	cfg := map[string]string{}

	// No clarity ratings and no review median: two of four criteria drop.
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}

	var included, dropped int
	var weightSum float64
	for _, c := range score.Criteria {
		if c.Included {
			included++
			weightSum += c.Weight
		} else {
			dropped++
			if c.Reason == "" {
				t.Errorf("criterion %q was dropped with no reason recorded", c.Key)
			}
			if c.Weight != 0 {
				t.Errorf("dropped criterion %q kept weight %v", c.Key, c.Weight)
			}
		}
	}
	if dropped < 2 {
		t.Fatalf("expected clarity and review-median to drop, got %d dropped", dropped)
	}
	// The surviving weights must still sum to 1, or the score silently
	// shrinks toward zero for every repo missing data.
	if weightSum < 0.999 || weightSum > 1.001 {
		t.Errorf("included weights sum to %v, want 1 after renormalisation", weightSum)
	}

	// A dropped clarity criterion must not be indistinguishable from a
	// terrible one.
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity {
			if c.Included {
				t.Error("clarity was scored with zero ratings")
			}
			if !strings.Contains(c.Reason, "rather than scored as zero") {
				t.Errorf("clarity drop reason does not explain the renormalisation: %q", c.Reason)
			}
		}
	}
}

// One rating must not decide a quarter of someone's payout.
func TestComputeMaintainerScore_ClarityNeedsAMinimumSample(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	cfg := map[string]string{"maintainer_clarity_min_ratings": "3"}

	clFxRated(t, pool, hackathonID, projectID, 1200, 5)
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity && c.Included {
			t.Fatal("clarity counted with a single rating")
		}
	}

	clFxRated(t, pool, hackathonID, projectID, 1201, 5)
	clFxRated(t, pool, hackathonID, projectID, 1202, 5)
	score, err = ComputeMaintainerScore(ctx, pool, hackathonID, projectID, nil, cfg)
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	var found bool
	for _, c := range score.Criteria {
		if c.Key == CriterionIssueClarity {
			found = c.Included
			if c.Raw == nil || *c.Raw != 5 {
				t.Errorf("clarity raw = %v, want the 5-star mean snapshotted", c.Raw)
			}
		}
	}
	if !found {
		t.Error("clarity did not count once it reached the minimum sample")
	}
}

// Every criterion value is snapshotted, not just the non-reproducible one.
// First-timer counts and clarity aggregates drift as data syncs too, and a
// score that comes back different during an appeal turns the appeal into an
// argument about arithmetic.
func TestComputeMaintainerScore_SnapshotsEveryInput(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	median := 12.0
	score, err := ComputeMaintainerScore(ctx, pool, hackathonID, projectID, &median, map[string]string{})
	if err != nil {
		t.Fatalf("ComputeMaintainerScore: %v", err)
	}
	if len(score.Criteria) != 4 {
		t.Fatalf("got %d criteria, want all 4 recorded even when some are dropped", len(score.Criteria))
	}
	for _, c := range score.Criteria {
		if c.Included && c.Raw == nil {
			t.Errorf("criterion %q was scored with no raw value snapshotted", c.Key)
		}
	}
}

// §7's structural guarantee: maintainer payouts never draw on the contributor
// pool. If someone later "simplifies" the two pools into one, this must fail.
func TestAllocateMaintainerPool_NeverDrawsFromTheContributorPool(t *testing.T) {
	scores := []MaintainerScore{
		{ProjectID: uuid.New(), OrgLogin: "acme", Score: 0.9},
		{ProjectID: uuid.New(), OrgLogin: "globex", Score: 0.1},
	}
	cfg := map[string]string{}
	now := time.Now()

	// An empty maintainer pool pays nothing, no matter how large the
	// contributor pool is - there is no input to this function that could
	// carry contributor money into it.
	for _, pool := range []float64{0} {
		payouts := AllocateMaintainerPool(scores, pool, cfg, now)
		for _, p := range payouts {
			if p.GrossAmount != 0 || p.ImmediateAmount != 0 || p.HoldbackAmount != 0 {
				t.Fatalf("maintainer paid %v from an empty maintainer pool", p.GrossAmount)
			}
		}
	}

	// And with a real maintainer pool, the total never exceeds it.
	const maintainerPool = 1000.0
	payouts := AllocateMaintainerPool(scores, maintainerPool, cfg, now)
	var total float64
	for _, p := range payouts {
		total += p.GrossAmount
		if p.ImmediateAmount+p.HoldbackAmount > p.GrossAmount+0.01 {
			t.Errorf("%s: immediate+holdback %v exceeds gross %v", p.OrgLogin, p.ImmediateAmount+p.HoldbackAmount, p.GrossAmount)
		}
	}
	if total > maintainerPool+0.01 {
		t.Fatalf("maintainer payouts total %v, which exceeds the maintainer pool of %v", total, maintainerPool)
	}
}

func TestAllocateMaintainerPool_HoldsBackTheConfiguredShare(t *testing.T) {
	scores := []MaintainerScore{{ProjectID: uuid.New(), OrgLogin: "acme", Score: 1}}
	settled := time.Now()
	payouts := AllocateMaintainerPool(scores, 1000, map[string]string{}, settled)
	p := payouts[0]

	if p.HoldbackPct != 30 {
		t.Errorf("holdback_pct = %d, want the shipped default of 30", p.HoldbackPct)
	}
	if p.HoldbackAmount != 300 {
		t.Errorf("holdback = %v, want 300", p.HoldbackAmount)
	}
	if p.ImmediateAmount != 700 {
		t.Errorf("immediate = %v, want 700", p.ImmediateAmount)
	}
	wantDue := settled.AddDate(0, 0, 90)
	if p.HoldbackDueAt.Sub(wantDue) > time.Minute || wantDue.Sub(p.HoldbackDueAt) > time.Minute {
		t.Errorf("holdback due %v, want ~%v (90 days)", p.HoldbackDueAt, wantDue)
	}
}

// The holdback must be conditional on continued activity. A timer alone pays
// the farmer three months late, which is the behaviour it exists to catch.
func TestDecideHoldback_ReleaseDependsOnContinuedActivity(t *testing.T) {
	payout := MaintainerPayout{HoldbackAmount: 300}
	cfg := map[string]string{}

	sustained := DecideHoldback(payout, RepoActivity{WindowDays: 60, Commits: 12, MergedPRs: 3, CommitsMeasured: true}, cfg)
	if sustained.Status != "released" || sustained.Released != 300 || sustained.Withheld != 0 {
		t.Errorf("sustained activity: %+v, want full release", sustained)
	}

	some := DecideHoldback(payout, RepoActivity{WindowDays: 60, Commits: 1, CommitsMeasured: true}, cfg)
	if some.Status != "partially_released" {
		t.Errorf("some activity: status = %q, want partially_released", some.Status)
	}
	if some.Released != 150 || some.Withheld != 150 {
		t.Errorf("some activity: released %v / withheld %v, want 150/150 at the default 50%%", some.Released, some.Withheld)
	}

	none := DecideHoldback(payout, RepoActivity{WindowDays: 60, CommitsMeasured: true}, cfg)
	if none.Status != "withheld" || none.Withheld != 300 {
		t.Errorf("no activity: %+v, want the holdback withheld", none)
	}
	if none.Destination != "next_event_pool" {
		t.Errorf("withheld destination = %q, want the published default", none.Destination)
	}

	// Failing to look is not evidence of absence.
	unmeasured := DecideHoldback(payout, RepoActivity{WindowDays: 60, CommitsMeasured: false}, cfg)
	if unmeasured.Status != "pending" {
		t.Errorf("unmeasured activity: status = %q, want pending - money must not be withheld because a GitHub call failed", unmeasured.Status)
	}
	if unmeasured.Withheld != 0 {
		t.Errorf("unmeasured activity withheld %v", unmeasured.Withheld)
	}
}
