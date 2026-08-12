package hackathon

import "fmt"

// PoolSplit is a sponsor's total broken into the three published numbers.
//
// The whole point of a fee is that it is disclosed. A net-only figure is a
// skim, so all three travel together and are stored together - there is no
// representation of a split here that omits the fee.
type PoolSplit struct {
	SponsorTotal float64 `json:"sponsor_total_usdc"`
	PlatformFee  float64 `json:"platform_fee_usdc"`
	// Net is what actually pays people: SponsorTotal - PlatformFee.
	Net float64 `json:"net_pool_usdc"`

	ContributorPool float64 `json:"contributor_prize_pool"`
	MaintainerPool  float64 `json:"maintainer_prize_pool"`

	// The rates that produced the numbers above, snapshotted so the split can
	// be re-derived from a stored row after the config has moved on.
	FeeRatePct         float64 `json:"platform_fee_rate_pct"`
	MaintainerSharePct float64 `json:"maintainer_share_pct"`
}

// SplitSponsorTotal applies the platform fee once, then divides the remainder.
//
//	fee         = round(total x fee_rate)
//	net         = total - fee
//	maintainer  = round(net x maintainer_share)
//	contributor = net - maintainer        <- takes the remainder
//
// The contributor pool absorbing the remainder is what makes the parts sum to
// the total exactly. Rounding each of the three independently would leave
// cents unassigned, and unassigned cents in a pool that is published as fully
// allocated is the kind of discrepancy nobody can explain afterwards.
//
// The fee is taken **once, off the total**, not per pool. A fee applied to the
// maintainer pool separately would entangle with the 30% holdback that
// releases 90 days later, and fee arithmetic that has to be re-run against a
// conditional release months after settlement is arithmetic that will go
// wrong.
func SplitSponsorTotal(sponsorTotal float64, cfg map[string]string) (PoolSplit, error) {
	if sponsorTotal < 0 {
		return PoolSplit{}, fmt.Errorf("hackathon.SplitSponsorTotal: negative sponsor total")
	}

	feePct := clampPct(atofOr(cfg["platform_fee_pct"], 0))
	maintPct := clampPct(atofOr(cfg["maintainer_share_pct"], 20))

	fee := round2(sponsorTotal * feePct / 100)
	net := round2(sponsorTotal - fee)
	maintainer := round2(net * maintPct / 100)
	contributor := round2(net - maintainer)

	return PoolSplit{
		SponsorTotal:       round2(sponsorTotal),
		PlatformFee:        fee,
		Net:                net,
		ContributorPool:    contributor,
		MaintainerPool:     maintainer,
		FeeRatePct:         feePct,
		MaintainerSharePct: maintPct,
	}, nil
}

// Reconciles reports whether the parts sum back to the total.
//
// Asserted by the caller before writing, and by a CHECK constraint in the
// database. Two independent checks on purpose: a fee that silently fails to
// add up is indistinguishable from a skim, and this is the one arithmetic
// where "probably fine" is not good enough.
func (s PoolSplit) Reconciles() bool {
	diff := s.SponsorTotal - s.PlatformFee - s.ContributorPool - s.MaintainerPool
	return diff < 0.011 && diff > -0.011
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
