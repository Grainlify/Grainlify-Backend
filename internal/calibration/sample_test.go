package calibration

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// fakePool builds a candidate pool shaped like the real one: three projects
// holding almost everything, two small ones, and one with a single PR - and
// mostly merged, as ours is (1299 of 1569).
func fakePool(t *testing.T) ([]Candidate, SizeFn) {
	t.Helper()
	shape := []struct {
		project  string
		total    int
		unmerged int
	}{
		{"Stellopay/stellopay-core", 60, 12},
		{"Stellopay/stellopay-frontend", 60, 15},
		{"Stellopay/stellopay-backend", 40, 6},
		{"MillestoneX/MilestoneX-Contracts", 12, 2},
		{"ValoraSec/valorasec", 10, 8},
		{"MillestoneX/MilestoneX-Backend", 1, 0},
	}
	var out []Candidate
	sizes := map[uuid.UUID]int{}
	n := 0
	for _, s := range shape {
		for i := 0; i < s.total; i++ {
			// Deterministic ids so the test is reproducible run to run.
			id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s-%d", s.project, i)))
			out = append(out, Candidate{
				PullRequestID:   id,
				ProjectFullName: s.project,
				Number:          i + 1,
				Merged:          i >= s.unmerged,
			})
			// A spread of sizes across all five bands.
			switch n % 5 {
			case 0:
				sizes[id] = 3
			case 1:
				sizes[id] = 30
			case 2:
				sizes[id] = 150
			case 3:
				sizes[id] = 600
			case 4:
				sizes[id] = 3000
			}
			n++
		}
	}
	return out, func(_ context.Context, c Candidate) (int, error) { return sizes[c.PullRequestID], nil }
}

func TestDrawSample_IsDeterministicForASeed(t *testing.T) {
	pool, size := fakePool(t)

	a, err := DrawSample(context.Background(), pool, DefaultPlan(), 1234, size, nil)
	if err != nil {
		t.Fatalf("draw a: %v", err)
	}
	b, err := DrawSample(context.Background(), pool, DefaultPlan(), 1234, size, nil)
	if err != nil {
		t.Fatalf("draw b: %v", err)
	}

	if len(a.Selected) != len(b.Selected) {
		t.Fatalf("sizes differ: %d vs %d", len(a.Selected), len(b.Selected))
	}
	for i := range a.Selected {
		if a.Selected[i].PullRequestID != b.Selected[i].PullRequestID {
			t.Fatalf("same seed produced a different sample at position %d", i)
		}
		if a.Selected[i].HeldBack != b.Selected[i].HeldBack {
			t.Fatalf("hold-back differs at position %d; it must be part of the seeded draw", i)
		}
	}
	if a.CandidateHash != b.CandidateHash {
		t.Error("candidate hash is not stable")
	}
}

// A different seed must produce a different sample, or the seed is decorative.
func TestDrawSample_DifferentSeedDiffers(t *testing.T) {
	pool, size := fakePool(t)
	a, _ := DrawSample(context.Background(), pool, DefaultPlan(), 1, size, nil)
	b, _ := DrawSample(context.Background(), pool, DefaultPlan(), 2, size, nil)

	same := 0
	for i := range a.Selected {
		for j := range b.Selected {
			if a.Selected[i].PullRequestID == b.Selected[j].PullRequestID {
				same++
			}
		}
	}
	if same == len(a.Selected) {
		t.Error("two seeds produced identical samples")
	}
}

// The order rows arrive in must not change the draw. Without the canonical
// sort before shuffling, the sample would depend on the database's row order.
func TestDrawSample_IgnoresInputOrder(t *testing.T) {
	pool, size := fakePool(t)
	reversed := make([]Candidate, len(pool))
	for i := range pool {
		reversed[len(pool)-1-i] = pool[i]
	}

	a, _ := DrawSample(context.Background(), pool, DefaultPlan(), 99, size, nil)
	b, _ := DrawSample(context.Background(), reversed, DefaultPlan(), 99, size, nil)

	for i := range a.Selected {
		if a.Selected[i].PullRequestID != b.Selected[i].PullRequestID {
			t.Fatalf("input order changed the draw at position %d", i)
		}
	}
}

func TestDrawSample_RespectsTheStrata(t *testing.T) {
	pool, size := fakePool(t)
	plan := DefaultPlan()

	d, err := DrawSample(context.Background(), pool, plan, 7, size, nil)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	byProject, byBand, merged, unmerged, held := d.Composition()

	if len(d.Selected) != plan.Total {
		t.Errorf("drew %d, want %d", len(d.Selected), plan.Total)
	}
	for p, n := range byProject {
		if n > plan.PerProjectCap {
			t.Errorf("%s contributed %d, over the cap of %d", p, n, plan.PerProjectCap)
		}
	}
	if unmerged < plan.UnmergedFloor {
		t.Errorf("unmerged = %d, below the floor of %d (relaxations: %v)", unmerged, plan.UnmergedFloor, d.Relaxations)
	}
	if unmerged > plan.UnmergedCeiling {
		t.Errorf("unmerged = %d, above the ceiling of %d - a target constrains both directions, and without the ceiling this drew 15 of 25", unmerged, plan.UnmergedCeiling)
	}
	if merged+unmerged != plan.Total {
		t.Errorf("outcomes do not sum: %d + %d != %d", merged, unmerged, plan.Total)
	}
	if held != plan.HoldBack {
		t.Errorf("held back %d, want %d", held, plan.HoldBack)
	}
	// Every band represented: the point of stratifying is that one-liners and
	// enormous PRs both appear, not that the middle is well covered.
	for _, b := range []SizeBand{BandTiny, BandSmall, BandMedium, BandLarge, BandHuge} {
		if byBand[b] == 0 {
			t.Errorf("band %s is empty; the sample is not stratified by size", b)
		}
	}
}

// A second draw must not reuse the first's pull requests.
func TestDrawSample_SecondDrawDoesNotOverlap(t *testing.T) {
	pool, size := fakePool(t)

	first, err := DrawSample(context.Background(), pool, DefaultPlan(), 11, size, nil)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	exclude := map[uuid.UUID]bool{}
	for _, s := range first.Selected {
		exclude[s.PullRequestID] = true
	}

	second, err := DrawSample(context.Background(), pool, DefaultPlan(), 12, size, exclude)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	for _, s := range second.Selected {
		if exclude[s.PullRequestID] {
			t.Errorf("second draw reused %s from the first set", s.PullRequestID)
		}
	}
}

// Hold-back must be part of the draw, not a later choice, and must be a
// strict subset - never the whole sample and never empty.
func TestDrawSample_HoldBackIsDrawnNotChosenLater(t *testing.T) {
	pool, size := fakePool(t)
	plan := DefaultPlan()
	d, err := DrawSample(context.Background(), pool, plan, 5150, size, nil)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}

	var held, open int
	for _, s := range d.Selected {
		if s.HeldBack {
			held++
		} else {
			open++
		}
	}
	if held != plan.HoldBack {
		t.Fatalf("held = %d, want %d", held, plan.HoldBack)
	}
	if open != plan.Total-plan.HoldBack {
		t.Fatalf("labellable = %d, want %d", open, plan.Total-plan.HoldBack)
	}
}

// A candidate whose size cannot be read is skipped, not fatal. One
// unreachable pull request should not cost the whole sample.
func TestDrawSample_UnreadableCandidateIsSkipped(t *testing.T) {
	pool, size := fakePool(t)
	failing := pool[0].PullRequestID
	wrapped := func(ctx context.Context, c Candidate) (int, error) {
		if c.PullRequestID == failing {
			return 0, fmt.Errorf("simulated GitHub 404")
		}
		return size(ctx, c)
	}

	d, err := DrawSample(context.Background(), pool, DefaultPlan(), 3, wrapped, nil)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	for _, s := range d.Selected {
		if s.PullRequestID == failing {
			t.Error("a candidate whose size could not be read was selected anyway")
		}
	}
}

func TestClassifySize_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		lines int
		want  SizeBand
	}{
		{1, BandTiny}, {5, BandTiny}, {6, BandSmall}, {50, BandSmall},
		{51, BandMedium}, {250, BandMedium}, {251, BandLarge}, {1000, BandLarge},
		{1001, BandHuge},
	} {
		if got := ClassifySize(tc.lines); got != tc.want {
			t.Errorf("ClassifySize(%d) = %s, want %s", tc.lines, got, tc.want)
		}
	}
}

// The pool fingerprint must not depend on the order ids arrive in, or a
// re-query that returns the same rows differently would look like a changed
// pool.
func TestHashCandidates_IsOrderIndependent(t *testing.T) {
	a := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	b := []uuid.UUID{a[2], a[0], a[1]}
	if HashCandidates(a) != HashCandidates(b) {
		t.Error("candidate hash depends on order")
	}
	if HashCandidates(a) == HashCandidates(append(a, uuid.New())) {
		t.Error("adding a candidate did not change the hash")
	}
}

// TestDrawSample_UnmergedIsATargetNotAFloor is the regression test for the
// distinction.
//
// The first version had a floor only. It satisfied "at least 10" and drew 15
// of 25 - 60% unmerged against a corpus that is 17% - because after the floor
// was met the later passes kept taking whichever candidate came next. The set
// was not wrong, but an agreement rate measured on it would not be comparable
// to the mix a model sees, which is the thing the sample exists to predict.
func TestDrawSample_UnmergedIsATargetNotAFloor(t *testing.T) {
	pool, size := fakePool(t)
	plan := DefaultPlan()

	// Several seeds, because one seed passing proves nothing about a bound.
	for _, seed := range []int64{1, 2, 3, 20260813, 99999} {
		d, err := DrawSample(context.Background(), pool, plan, seed, size, nil)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		_, _, _, unmerged, _ := d.Composition()
		if unmerged < plan.UnmergedFloor || unmerged > plan.UnmergedCeiling {
			t.Errorf("seed %d: unmerged = %d, outside the target %d-%d",
				seed, unmerged, plan.UnmergedFloor, plan.UnmergedCeiling)
		}
	}
}

// A ceiling below the floor is a contradiction, not something to resolve
// silently in one direction.
func TestDrawSample_RejectsAContradictoryTarget(t *testing.T) {
	pool, size := fakePool(t)
	plan := DefaultPlan()
	plan.UnmergedFloor, plan.UnmergedCeiling = 12, 10

	if _, err := DrawSample(context.Background(), pool, plan, 1, size, nil); err == nil {
		t.Error("a ceiling below the floor was accepted")
	}
}

// The corpus proportion travels with the draw, so a number read later sits
// next to what it was measured against.
func TestDrawSample_RecordsTheCorpusProportion(t *testing.T) {
	pool, size := fakePool(t)
	d, err := DrawSample(context.Background(), pool, DefaultPlan(), 42, size, nil)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if d.CorpusTotal != len(pool) {
		t.Errorf("CorpusTotal = %d, want %d", d.CorpusTotal, len(pool))
	}
	wantUnmerged := 0
	for _, c := range pool {
		if !c.Merged {
			wantUnmerged++
		}
	}
	if d.CorpusUnmerged != wantUnmerged {
		t.Errorf("CorpusUnmerged = %d, want %d", d.CorpusUnmerged, wantUnmerged)
	}
	if d.CorpusUnmerged == 0 || d.CorpusUnmerged == d.CorpusTotal {
		t.Error("fixture is degenerate; the proportion proves nothing")
	}
}

// TestDrawSample_SkipsPullRequestsThatChangeNothing.
//
// A PR with no additions and no deletions gives a labeller nothing to judge,
// so it burns a slot in a 25-row sample. Found in the first real draw:
// MilestoneX-Backend#1 is merged and changes zero lines.
func TestDrawSample_SkipsPullRequestsThatChangeNothing(t *testing.T) {
	pool, size := fakePool(t)
	empty := pool[3].PullRequestID
	wrapped := func(ctx context.Context, c Candidate) (int, error) {
		if c.PullRequestID == empty {
			return 0, nil
		}
		return size(ctx, c)
	}

	d, err := DrawSample(context.Background(), pool, DefaultPlan(), 20260813, wrapped, nil)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	for _, s := range d.Selected {
		if s.PullRequestID == empty {
			t.Error("a pull request changing zero lines was selected; there is nothing to label")
		}
		if s.ChangedLines == 0 {
			t.Errorf("%s #%d was selected with 0 changed lines", s.ProjectFullName, s.Number)
		}
	}
}
