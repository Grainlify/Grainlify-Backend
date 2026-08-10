package hackathon

import (
	"fmt"
	"math"
	"sort"
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
	VerdictID string  `json:"verdict_id"`
	Login     string  `json:"github_login"`
	Bucket    string  `json:"bucket"`
	Units     int     `json:"units"`
	Amount    float64 `json:"amount"`
	// Funded is false when the payout floor exhausted the pool before
	// reaching this entry. It is never silently paid a token amount.
	Funded bool `json:"funded"`
}

// PayoutPlan is a computed, unpublished payout. §5.8 plus §12's shadow-mode
// rollout: computing and publishing are separate acts, and the first event
// is meant to compute this and pay by hand.
type PayoutPlan struct {
	TotalUnits    int           `json:"total_units"`
	UnitValue     float64       `json:"unit_value"`
	Pool          float64       `json:"contributor_prize_pool"`
	FloorApplied  bool          `json:"floor_applied"`
	PayoutFloor   float64       `json:"payout_floor"`
	Entries       []PayoutEntry `json:"entries"`
	UnfundedCount int           `json:"unfunded_count"`
	Note          string        `json:"note,omitempty"`
}

// JudgedPR is one non-rejected submission entering the payout maths.
type JudgedPR struct {
	VerdictID string
	Login     string
	Bucket    string
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
			})
			continue
		}
		funded = append(funded, pr)
		plan.TotalUnits += units
	}

	if plan.TotalUnits == 0 {
		plan.Note = "No non-rejected submissions, so there is nothing to divide."
		return plan, nil
	}

	plan.UnitValue = pool / float64(plan.TotalUnits)

	if plan.UnitValue >= floor || floor <= 0 {
		for _, pr := range funded {
			units := UnitsFor(pr.Bucket, cfg)
			plan.Entries = append(plan.Entries, PayoutEntry{
				VerdictID: pr.VerdictID, Login: pr.Login, Bucket: pr.Bucket,
				Units: units, Amount: round2(float64(units) * plan.UnitValue), Funded: true,
			})
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
		for _, pr := range byBucket[bucket] {
			units := UnitsFor(pr.Bucket, cfg)
			amount := round2(float64(units) * floor)
			entry := PayoutEntry{
				VerdictID: pr.VerdictID, Login: pr.Login, Bucket: pr.Bucket, Units: units,
			}
			if amount <= remaining {
				entry.Amount = amount
				entry.Funded = true
				remaining -= amount
			} else {
				plan.UnfundedCount++
			}
			plan.Entries = append(plan.Entries, entry)
		}
	}
	plan.Note = fmt.Sprintf(
		"Unit value would have been %.2f, below the %.2f floor. Applied %s: paid at the floor, highest buckets first, until the pool was exhausted.",
		pool/float64(plan.TotalUnits), floor, strategy)

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
