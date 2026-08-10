package hackathon

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BucketOrder is the buckets from strongest to weakest. Used by the payout
// floor, which funds the highest buckets first.
var BucketOrder = []string{"exceptional", "substantial", "accepted", "rejected"}

// UnitsFor resolves a bucket's unit value from config (§3.10).
func UnitsFor(bucket string, cfg map[string]string) int {
	switch bucket {
	case "exceptional":
		return atoiOr(cfg["units_exceptional"], 5)
	case "substantial":
		return atoiOr(cfg["units_substantial"], 3)
	case "accepted":
		return atoiOr(cfg["units_accepted"], 1)
	default:
		return atoiOr(cfg["units_rejected"], 0)
	}
}

// PayoutEntry is one contributor's share of a payout run.
type PayoutEntry struct {
	VerdictID string `json:"verdict_id"`
	Login     string `json:"github_login"`
	Bucket    string `json:"bucket"`
	// Units is the bucket's base unit value, before the curve.
	Units int `json:"units"`
	// CurvePosition is this PR's 1-based position among the contributor's
	// funded PRs, ordered by merge time; CurveMultiplier is the multiplier
	// that position attracted. Both are stored so an appeal can see why a PR
	// earned what it did rather than having to reconstruct the ordering.
	CurvePosition   int     `json:"curve_position"`
	CurveMultiplier float64 `json:"curve_multiplier"`
	// EffectiveUnits is Units x CurveMultiplier - what actually divided the
	// pool.
	EffectiveUnits float64 `json:"effective_units"`
	Amount         float64 `json:"amount"`
	// Funded is false when the payout floor exhausted the pool before
	// reaching this entry. It is never silently paid a token amount.
	Funded bool `json:"funded"`
}

// PayoutPlan is a computed, unpublished payout. §5.8 plus §12's shadow-mode
// rollout: computing and publishing are separate acts, and the first event
// is meant to compute this and pay by hand.
type PayoutPlan struct {
	// TotalUnits is fractional once the curve applies: a second PR at 0.8 of
	// a 3-unit bucket contributes 2.4.
	TotalUnits    float64       `json:"total_units"`
	UnitValue     float64       `json:"unit_value"`
	Pool          float64       `json:"contributor_prize_pool"`
	FloorApplied  bool          `json:"floor_applied"`
	PayoutFloor   float64       `json:"payout_floor"`
	Entries       []PayoutEntry `json:"entries"`
	UnfundedCount int           `json:"unfunded_count"`
	// CurveApplied and Curve record the diminishing-returns settings this run
	// used, so a payout is reproducible from its own snapshot even if the
	// config changes afterwards.
	CurveApplied bool      `json:"curve_applied"`
	Curve        []float64 `json:"curve,omitempty"`
	Note         string    `json:"note,omitempty"`
}

// JudgedPR is one non-rejected submission entering the payout maths.
type JudgedPR struct {
	VerdictID string
	Login     string
	Bucket    string
	// MergedAt orders a contributor's PRs for the diminishing-returns curve.
	// Zero times sort first and then by VerdictID, so a missing merge time
	// still produces a stable, reproducible ordering rather than one that
	// depends on row order.
	MergedAt time.Time
}

// DefaultDiminishingCurve is the multiplier applied to a contributor's 1st,
// 2nd, 3rd... funded PR. The final value repeats for every PR beyond the list.
var DefaultDiminishingCurve = []float64{1.0, 0.8, 0.6, 0.5, 0.4}

// ParseDiminishingCurve reads diminishing_returns_curve, falling back to the
// default on anything it cannot parse.
//
// Falls back rather than erroring because this runs inside a payout: a
// malformed curve must not stop people being paid, and the default is a
// published, conservative shape. A parse failure that silently produced an
// empty curve would instead multiply every PR by nothing.
func ParseDiminishingCurve(raw string) []float64 {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if raw == "" {
		return DefaultDiminishingCurve
	}
	var out []float64
	for _, part := range strings.Split(raw, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || v < 0 {
			return DefaultDiminishingCurve
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return DefaultDiminishingCurve
	}
	return out
}

// curveMultiplier returns the multiplier for a 1-based position, repeating
// the last value for anything beyond the curve.
func curveMultiplier(curve []float64, position int) float64 {
	if len(curve) == 0 {
		return 1
	}
	if position < 1 {
		position = 1
	}
	if position > len(curve) {
		return curve[len(curve)-1]
	}
	return curve[position-1]
}

// applyDiminishingReturns assigns each funded PR its curve position and
// multiplier, in place.
//
// Positions are per contributor, ordered by merge time, and counted over
// funded PRs only - a rejected PR earns nothing and must not consume a
// position, or a contributor would be penalised for having submitted work
// that was thrown out.
//
// Logins are grouped case-insensitively. GitHub logins vary in capitalisation
// across rows, and grouping on the raw value would give the same person two
// independent curves - which is precisely the loophole the curve exists to
// close.
func applyDiminishingReturns(funded []JudgedPR, curve []float64, unitsOf func(JudgedPR) int) map[string]PayoutEntry {
	byLogin := map[string][]JudgedPR{}
	for _, pr := range funded {
		key := strings.ToLower(pr.Login)
		byLogin[key] = append(byLogin[key], pr)
	}

	out := make(map[string]PayoutEntry, len(funded))
	for _, prs := range byLogin {
		sort.SliceStable(prs, func(a, b int) bool {
			if !prs[a].MergedAt.Equal(prs[b].MergedAt) {
				return prs[a].MergedAt.Before(prs[b].MergedAt)
			}
			return prs[a].VerdictID < prs[b].VerdictID
		})
		for i, pr := range prs {
			pos := i + 1
			mult := curveMultiplier(curve, pos)
			units := unitsOf(pr)
			out[pr.VerdictID] = PayoutEntry{
				VerdictID: pr.VerdictID, Login: pr.Login, Bucket: pr.Bucket,
				Units: units, CurvePosition: pos, CurveMultiplier: mult,
				EffectiveUnits: float64(units) * mult,
			}
		}
	}
	return out
}

// ComputePayout implements AI-specs.md §5.8. Plain code, no AI.
//
//	total_units = Σ(units per non-rejected PR)
//	unit_value  = contributor_prize_pool / total_units
//	payout      = units × unit_value
//
// The floor is the part that matters. §5.8: "If unit_value falls below
// payout_floor, do not silently pay $9 to a valid contribution. Apply
// payout_floor_strategy - fund the highest buckets first until the pool is
// exhausted at the floor value - and publish this rule in advance. Deciding
// it after the numbers are in is how retro-funding programs generate their
// worst press."
func ComputePayout(prs []JudgedPR, pool float64, cfg map[string]string) (*PayoutPlan, error) {
	if pool < 0 {
		return nil, fmt.Errorf("hackathon.ComputePayout: negative prize pool")
	}
	floor := atofOr(cfg["payout_floor"], 50)

	plan := &PayoutPlan{Pool: pool, PayoutFloor: floor}

	// Rejected PRs are worth zero units by definition and take no share.
	var funded []JudgedPR
	for _, pr := range prs {
		units := UnitsFor(pr.Bucket, cfg)
		if units <= 0 {
			plan.Entries = append(plan.Entries, PayoutEntry{
				VerdictID: pr.VerdictID, Login: pr.Login, Bucket: pr.Bucket, Units: units,
				CurveMultiplier: 0,
			})
			continue
		}
		funded = append(funded, pr)
	}

	// Diminishing returns, applied after buckets are final and before the
	// pool is divided.
	//
	// The reduced units raise unit_value for everyone - that redistribution
	// is the point, not a side effect. It replaces a hard cap on how many
	// issues one person may take: instead of refusing their sixth PR, it
	// pays it less and hands the difference to everybody else.
	//
	// A contributor cannot work out their multiplier during the event,
	// because a PR's position depends on how many they end up merging. That
	// is deliberate and is stated on the rules page.
	curve := DefaultDiminishingCurve
	curveOn := cfg["diminishing_returns_enabled"] != "false"
	if curveOn {
		curve = ParseDiminishingCurve(cfg["diminishing_returns_curve"])
	}
	unitsOf := func(pr JudgedPR) int { return UnitsFor(pr.Bucket, cfg) }
	shaped := applyDiminishingReturns(funded, curve, unitsOf)
	if !curveOn {
		// Still record a position, so a snapshot taken with the curve off is
		// readable in the same way as one taken with it on.
		for id, e := range shaped {
			e.CurveMultiplier = 1
			e.EffectiveUnits = float64(e.Units)
			shaped[id] = e
		}
	}
	for _, e := range shaped {
		plan.TotalUnits += e.EffectiveUnits
	}
	plan.CurveApplied = curveOn
	plan.Curve = curve

	if plan.TotalUnits <= 0 {
		plan.Note = "No non-rejected submissions, so there is nothing to divide."
		return plan, nil
	}

	plan.UnitValue = pool / plan.TotalUnits

	// The floor is checked against the post-curve unit value, so a
	// contributor whose later PRs shrink below it is handled by
	// payout_floor_strategy rather than quietly paid a token amount.
	if plan.UnitValue >= floor || floor <= 0 {
		for _, pr := range funded {
			e := shaped[pr.VerdictID]
			e.Amount = round2(e.EffectiveUnits * plan.UnitValue)
			e.Funded = true
			plan.Entries = append(plan.Entries, e)
		}
		sortEntries(plan.Entries)
		return plan, nil
	}

	// Floor strategy: pay at the floor, highest buckets first, until the
	// pool runs out. Everything past that point is explicitly unfunded
	// rather than paid a meaningless amount.
	plan.FloorApplied = true
	plan.UnitValue = floor
	strategy := cfg["payout_floor_strategy"]
	if strategy == "" {
		strategy = "fund_highest_buckets_first"
	}

	byBucket := map[string][]JudgedPR{}
	for _, pr := range funded {
		byBucket[pr.Bucket] = append(byBucket[pr.Bucket], pr)
	}

	remaining := pool
	for _, bucket := range BucketOrder {
		// Within a bucket, earlier curve positions are funded first. A
		// contributor's sixth PR should not outrank someone else's first when
		// the pool is running out.
		inBucket := byBucket[bucket]
		sort.SliceStable(inBucket, func(a, b int) bool {
			return shaped[inBucket[a].VerdictID].CurvePosition < shaped[inBucket[b].VerdictID].CurvePosition
		})
		for _, pr := range inBucket {
			e := shaped[pr.VerdictID]
			amount := round2(e.EffectiveUnits * floor)
			if amount > 0 && amount <= remaining {
				e.Amount = amount
				e.Funded = true
				remaining -= amount
			} else {
				plan.UnfundedCount++
			}
			plan.Entries = append(plan.Entries, e)
		}
	}
	plan.Note = fmt.Sprintf(
		"Unit value would have been %.2f, below the %.2f floor. Applied %s: paid at the floor, highest buckets first and earliest PRs first within a bucket, until the pool was exhausted.",
		pool/plan.TotalUnits, floor, strategy)

	sortEntries(plan.Entries)
	return plan, nil
}

// sortEntries orders a plan for reading: funded before unfunded, then by
// amount, then by login so the output is stable across runs.
func sortEntries(entries []PayoutEntry) {
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Funded != entries[b].Funded {
			return entries[a].Funded
		}
		if entries[a].Amount != entries[b].Amount {
			return entries[a].Amount > entries[b].Amount
		}
		return entries[a].Login < entries[b].Login
	})
}

// round2 rounds to cents. Money is rounded once, at the point it becomes an
// amount, rather than accumulating float drift through the calculation.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
