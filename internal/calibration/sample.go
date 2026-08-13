// Package calibration is the internal hand-labelling tool. It is not part of
// the product: nothing in the product reads these tables, no product surface
// exposes these handlers, and the whole thing runs locally behind
// CALIBRATION_ENABLED.
//
// It exists so pull requests can be labelled by people, and the AI judged
// against those labels afterwards. That ordering matters - the labels are
// ground truth produced before the model has an opinion, which is why nothing
// here may ever surface a model output to a labeller.
package calibration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"sort"

	"github.com/google/uuid"
)

// Candidate is one pull request eligible for sampling, as the database knows
// it. Size is deliberately absent: we do not store additions/deletions, so it
// is discovered during the draw and recorded on the way through.
type Candidate struct {
	PullRequestID   uuid.UUID
	ProjectFullName string
	Number          int
	Merged          bool
}

// SizeBand buckets a pull request by how much it changes.
//
// Bands rather than raw counts because the sample is stratified on size and a
// stratum needs a name. The boundaries are judgement, not science: "tiny" is
// meant to catch genuine one-liners, "huge" the ones nobody reviews properly.
type SizeBand string

const (
	BandTiny   SizeBand = "tiny"   // <= 5 changed lines
	BandSmall  SizeBand = "small"  // 6-50
	BandMedium SizeBand = "medium" // 51-250
	BandLarge  SizeBand = "large"  // 251-1000
	BandHuge   SizeBand = "huge"   // > 1000
)

// ClassifySize buckets a changed-line count.
func ClassifySize(changedLines int) SizeBand {
	switch {
	case changedLines <= 5:
		return BandTiny
	case changedLines <= 50:
		return BandSmall
	case changedLines <= 250:
		return BandMedium
	case changedLines <= 1000:
		return BandLarge
	default:
		return BandHuge
	}
}

// Plan is what the draw is aiming for. Recorded on the sample so a later
// reader can see the constraints as given, rather than inferring them from
// the result.
type Plan struct {
	Total int `json:"total"`
	// PerProjectCap stops one repository dominating. Three Stellopay
	// repositories hold 97% of the indexed corpus, so without a cap the draw
	// is a Stellopay draw with rounding errors.
	PerProjectCap int `json:"per_project_cap"`
	// UnmergedFloor and UnmergedCeiling are a TARGET, not a floor - the
	// distinction is the whole reason both exist, and the names are chosen so
	// the next person cannot read one as the other.
	//
	// A floor alone guarantees a minimum and constrains nothing above it. That
	// is what this had first, and it drew 15 unmerged out of 25 - 60%, against
	// a corpus that is 17% unmerged - because once the floor was met the later
	// passes kept taking whichever candidate came next in the shuffle. The set
	// was not wrong, but an agreement rate measured on it would not have been
	// comparable to the mix a model actually sees.
	//
	// A target constrains both directions. Unmerged is still deliberately
	// over-sampled against the true 17%, because that is where human and model
	// disagree and where the hard middle lives - it just is not allowed to
	// dominate.
	UnmergedFloor   int `json:"unmerged_floor"`
	UnmergedCeiling int `json:"unmerged_ceiling"`
	// BandTargets is how many of each size band to aim for. They sum to Total.
	BandTargets map[SizeBand]int `json:"band_targets"`
	// HoldBack is how many of the drawn set are withheld from comparison runs
	// until explicitly released.
	HoldBack int `json:"hold_back"`
}

// DefaultPlan is the shape agreed for the first set: 25 PRs, no more than 6
// from any one project, at least 10 unmerged, 5 held back.
func DefaultPlan() Plan {
	return Plan{
		Total:           25,
		PerProjectCap:   6,
		UnmergedFloor:   10,
		UnmergedCeiling: 12,
		BandTargets: map[SizeBand]int{
			BandTiny:   3,
			BandSmall:  6,
			BandMedium: 8,
			BandLarge:  5,
			BandHuge:   3,
		},
		HoldBack: 5,
	}
}

// Selected is one drawn pull request with everything the draw learned about it.
type Selected struct {
	Candidate
	Band         SizeBand
	ChangedLines int
	HeldBack     bool
}

// SizeFn reports how many lines a candidate changes. Network-backed in real
// use; a map in tests. Returning an error drops that candidate from the draw
// rather than failing the whole run - one unreachable PR should not cost a
// sample.
type SizeFn func(ctx context.Context, c Candidate) (changedLines int, err error)

// Draw is the result of one sampling run.
type Draw struct {
	Seed          int64
	Plan          Plan
	Selected      []Selected
	CandidateIDs  []uuid.UUID
	CandidateHash string
	// Relaxations records any constraint the draw could not satisfy exactly,
	// in the order it was relaxed. Empty means every target was met.
	//
	// Recorded rather than silently absorbed: a sample that quietly missed its
	// size targets would produce a number nobody could interpret later.
	Relaxations []string
	// Examined is how many candidates were inspected to fill the sample. It is
	// the cost of the draw in API calls, and it says how deep into the shuffle
	// the constraints forced us.
	Examined int
	// CorpusTotal and CorpusUnmerged describe the pool the sample came out of.
	//
	// Recorded so an agreement rate read a year from now sits next to what it
	// was measured against. "82% agreement" means something different on a set
	// that is 17% unmerged and on one that is 48% unmerged, and nobody will
	// reconstruct the difference later from the sample alone.
	CorpusTotal    int
	CorpusUnmerged int
}

var (
	ErrNoCandidates   = errors.New("calibration: no candidates to draw from")
	ErrPlanImpossible = errors.New("calibration: plan cannot be satisfied by this candidate pool")
)

// HashCandidates fingerprints a candidate pool.
//
// Stored with the draw so that re-running the same seed against a changed pool
// is detectable. Without it, "reproducible" quietly means "reproducible until
// sync adds another pull request".
func HashCandidates(ids []uuid.UUID) string {
	sorted := append([]uuid.UUID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	h := sha256.New()
	for _, id := range sorted {
		h.Write(id[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DrawSample selects a stratified sample deterministically.
//
// Determinism comes from three things together: the candidate pool is sorted
// into a canonical order first, the shuffle is seeded, and the walk is greedy
// in shuffled order. The same pool and seed therefore always produce the same
// sample, whatever order the database returned rows in.
//
// exclude lets a later draw avoid everything an earlier one took, so a second
// set does not overlap the first.
func DrawSample(ctx context.Context, candidates []Candidate, plan Plan, seed int64, size SizeFn, exclude map[uuid.UUID]bool) (Draw, error) {
	if len(candidates) == 0 {
		return Draw{}, ErrNoCandidates
	}
	if plan.HoldBack > plan.Total {
		return Draw{}, fmt.Errorf("%w: hold-back %d exceeds total %d", ErrPlanImpossible, plan.HoldBack, plan.Total)
	}
	if plan.UnmergedCeiling > 0 && plan.UnmergedCeiling < plan.UnmergedFloor {
		return Draw{}, fmt.Errorf("%w: unmerged ceiling %d is below the floor %d", ErrPlanImpossible, plan.UnmergedCeiling, plan.UnmergedFloor)
	}

	// Canonical order before shuffling, so the draw does not inherit whatever
	// order the query happened to return.
	pool := make([]Candidate, 0, len(candidates))
	ids := make([]uuid.UUID, 0, len(candidates))
	for _, c := range candidates {
		if exclude[c.PullRequestID] {
			continue
		}
		pool = append(pool, c)
		ids = append(ids, c.PullRequestID)
	}
	if len(pool) == 0 {
		return Draw{}, ErrNoCandidates
	}
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].PullRequestID.String() < pool[j].PullRequestID.String()
	})

	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	d := Draw{Seed: seed, Plan: plan, CandidateIDs: ids, CandidateHash: HashCandidates(ids), CorpusTotal: len(pool)}
	for _, c := range pool {
		if !c.Merged {
			d.CorpusUnmerged++
		}
	}

	perProject := map[string]int{}
	perBand := map[SizeBand]int{}
	unmerged := 0

	// Three passes, in this order and for this reason:
	//
	//  A. unmerged only, until the floor is met. Unmerged work is the scarce
	//     stratum - 17% of the corpus, concentrated in two repositories - and
	//     it is where human and model disagree most. Taken first because a
	//     greedy walk that considers merged candidates alongside them spends
	//     the per-project cap on merged PRs and then cannot reach the floor.
	//     That is not hypothetical: it is what the first version of this did,
	//     landing on 9 of 10.
	//  B. any outcome, still honouring the size targets, until full.
	//  C. any outcome, size targets relaxed, until full. Size is the target
	//     most likely to be unsatisfiable, because we cannot see sizes until
	//     we have spent an API call on them.
	//
	// Each pass walks the same shuffled order, so the result stays
	// deterministic for a seed.
	type pass struct {
		unmergedOnly  bool
		honourBands   bool
		stopAtFloor   bool
		relaxationMsg string
	}
	passes := []pass{
		{unmergedOnly: true, honourBands: true, stopAtFloor: true},
		{honourBands: true},
		{relaxationMsg: "size band targets relaxed: the pool could not fill every band within the per-project cap"},
	}
	// (see seedOneOfEachBand below)
	// An EMPTY band is worse than an under-filled one. Missing three of six
	// "small" rows changes the balance; missing every one-liner means the set
	// does not test the trivial-change boundary at all, which is one of the
	// things stratifying by size is for. So before giving up on a band, the
	// walk looks specifically for it.
	//
	// This runs before the relaxing pass, so a band is only abandoned once the
	// pool genuinely has nothing left to fill it with - and it is deterministic
	// for a seed, because it walks the same shuffled order.
	seedBand := func(band SizeBand) {
		{
			if perBand[band] > 0 || len(d.Selected) >= plan.Total {
				return
			}
			for _, c := range pool {
				if len(d.Selected) >= plan.Total {
					break
				}
				if perProject[c.ProjectFullName] >= plan.PerProjectCap || containsID(d.Selected, c.PullRequestID) {
					continue
				}
				if !c.Merged && plan.UnmergedCeiling > 0 && unmerged >= plan.UnmergedCeiling {
					continue
				}
				lines, err := size(ctx, c)
				if err != nil {
					continue
				}
				d.Examined++
				if lines == 0 || ClassifySize(lines) != band {
					continue
				}
				d.Selected = append(d.Selected, Selected{Candidate: c, Band: band, ChangedLines: lines})
				perProject[c.ProjectFullName]++
				perBand[band]++
				if !c.Merged {
					unmerged++
				}
				break
			}
		}
	}

	// Seed one of each band FIRST. Running this later cannot work: by the time
	// the general passes finish, every slot is taken, and a fill pass with no
	// room to fill is a pass that silently does nothing - which is exactly what
	// set-2's first draw did, coming out with zero one-liners while reporting
	// only a generic relaxation.
	//
	// Bands are seeded in a fixed order so the draw stays deterministic.
	for _, band := range []SizeBand{BandTiny, BandSmall, BandMedium, BandLarge, BandHuge} {
		if _, wanted := plan.BandTargets[band]; wanted {
			seedBand(band)
		}
	}

	for _, ps := range passes {
		// Between the band-honouring pass and the relaxing one, make sure no
		// band is empty.
		if len(d.Selected) >= plan.Total {
			break
		}
		if ps.stopAtFloor && unmerged >= plan.UnmergedFloor {
			continue
		}
		took := 0
		for _, c := range pool {
			if len(d.Selected) >= plan.Total {
				break
			}
			if ps.stopAtFloor && unmerged >= plan.UnmergedFloor {
				break
			}
			if ps.unmergedOnly && c.Merged {
				continue
			}
			// The ceiling half of the target. Without it the floor is the only
			// constraint and unmerged work accumulates unbounded.
			if !c.Merged && plan.UnmergedCeiling > 0 && unmerged >= plan.UnmergedCeiling {
				continue
			}
			if perProject[c.ProjectFullName] >= plan.PerProjectCap {
				continue
			}
			if containsID(d.Selected, c.PullRequestID) {
				continue
			}

			lines, err := size(ctx, c)
			if err != nil {
				continue
			}
			d.Examined++
			// A pull request that changes nothing cannot be labelled: there is
			// no diff to judge, so a labeller can only guess and the slot is
			// wasted. Real - the first draw took MilestoneX-Backend#1, merged
			// with 0 additions and 0 deletions.
			if lines == 0 {
				continue
			}
			band := ClassifySize(lines)

			if ps.honourBands {
				if target, ok := plan.BandTargets[band]; ok && perBand[band] >= target {
					continue
				}
			}

			d.Selected = append(d.Selected, Selected{Candidate: c, Band: band, ChangedLines: lines})
			perProject[c.ProjectFullName]++
			perBand[band]++
			if !c.Merged {
				unmerged++
			}
			took++
		}
		if ps.relaxationMsg != "" && took > 0 {
			d.Relaxations = append(d.Relaxations, ps.relaxationMsg)
		}
	}

	if len(d.Selected) < plan.Total {
		return d, fmt.Errorf("%w: drew %d of %d after examining %d candidates", ErrPlanImpossible, len(d.Selected), plan.Total, d.Examined)
	}
	for band := range plan.BandTargets {
		if perBand[band] == 0 {
			d.Relaxations = append(d.Relaxations,
				fmt.Sprintf("band %q is EMPTY - the pool has no such pull request left within the per-project cap, so this set does not exercise that size at all", band))
		}
	}
	if unmerged < plan.UnmergedFloor {
		d.Relaxations = append(d.Relaxations,
			fmt.Sprintf("unmerged floor missed: %d of %d - the pool ran out of unmerged candidates within the per-project cap", unmerged, plan.UnmergedFloor))
	}

	// Hold-back is chosen here, at draw time, before anybody sees a pull
	// request. Chosen from the same seeded stream so it is reproducible, and
	// spread across the selection rather than taken off the end, which would
	// make it correlate with whatever the greedy walk happened to accept last.
	holdIdx := rng.Perm(len(d.Selected))[:plan.HoldBack]
	for _, i := range holdIdx {
		d.Selected[i].HeldBack = true
	}

	return d, nil
}

func containsID(sel []Selected, id uuid.UUID) bool {
	for _, s := range sel {
		if s.PullRequestID == id {
			return true
		}
	}
	return false
}

// Composition summarises a draw for reporting: counts by project, by band and
// by outcome. This is what gets read before the sample is frozen.
func (d Draw) Composition() (byProject map[string]int, byBand map[SizeBand]int, merged, unmerged, held int) {
	byProject = map[string]int{}
	byBand = map[SizeBand]int{}
	for _, s := range d.Selected {
		byProject[s.ProjectFullName]++
		byBand[s.Band]++
		if s.Merged {
			merged++
		} else {
			unmerged++
		}
		if s.HeldBack {
			held++
		}
	}
	return byProject, byBand, merged, unmerged, held
}
