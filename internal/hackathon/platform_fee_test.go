package hackathon

import (
	"math"
	"testing"
)

func feeCfg(feePct, maintPct string) map[string]string {
	return map[string]string{"platform_fee_pct": feePct, "maintainer_share_pct": maintPct}
}

// TestSplitSponsorTotal_DefaultTakesNothing. For money, safe means take
// nothing: an event that runs before anyone sets a rate must not quietly skim
// a built-in percentage. This is deliberately the opposite of how shadow mode
// defaults, and the reason is in the config comment.
func TestSplitSponsorTotal_DefaultTakesNothing(t *testing.T) {
	split, err := SplitSponsorTotal(10000, map[string]string{})
	if err != nil {
		t.Fatalf("SplitSponsorTotal: %v", err)
	}
	if split.PlatformFee != 0 {
		t.Errorf("default fee = %v, want 0 - an unset rate must take nothing", split.PlatformFee)
	}
	if split.Net != 10000 {
		t.Errorf("net = %v, want the whole 10000", split.Net)
	}
}

// TestSplitSponsorTotal_TakesTheFeeOnceOffTheTotal, not per pool. A fee on the
// maintainer pool separately would entangle with the 30% holdback that
// releases 90 days later.
func TestSplitSponsorTotal_TakesTheFeeOnceOffTheTotal(t *testing.T) {
	split, err := SplitSponsorTotal(10000, feeCfg("5", "20"))
	if err != nil {
		t.Fatalf("SplitSponsorTotal: %v", err)
	}
	if split.PlatformFee != 500 {
		t.Errorf("fee = %v, want 500 (5%% of 10000)", split.PlatformFee)
	}
	if split.Net != 9500 {
		t.Errorf("net = %v, want 9500", split.Net)
	}
	// The remainder splits: 20% of net to maintainers, the rest to contributors.
	if split.MaintainerPool != 1900 {
		t.Errorf("maintainer = %v, want 1900 (20%% of 9500)", split.MaintainerPool)
	}
	if split.ContributorPool != 7600 {
		t.Errorf("contributor = %v, want 7600", split.ContributorPool)
	}
	// The fee came off the total once - not 5% of each pool.
	if split.MaintainerPool+split.ContributorPool != split.Net {
		t.Errorf("pools sum to %v, want the net %v", split.MaintainerPool+split.ContributorPool, split.Net)
	}
}

// TestSplitSponsorTotal_AlwaysReconciles is the property the CHECK constraint
// also enforces. Unassigned cents in a pool published as fully allocated is
// the kind of discrepancy nobody can explain afterwards, so the contributor
// pool takes the remainder rather than being rounded independently.
func TestSplitSponsorTotal_AlwaysReconciles(t *testing.T) {
	// Deliberately awkward numbers: thirds, primes, and amounts whose
	// percentages do not land on whole cents.
	totals := []float64{0, 0.01, 1, 33.33, 99.99, 1000.005, 7777.77, 123456.78}
	rates := []string{"0", "0.5", "3", "5", "7.5", "33.333", "100"}
	shares := []string{"0", "17", "20", "33.333", "50", "100"}

	for _, total := range totals {
		for _, rate := range rates {
			for _, share := range shares {
				split, err := SplitSponsorTotal(total, feeCfg(rate, share))
				if err != nil {
					t.Fatalf("SplitSponsorTotal(%v, %s, %s): %v", total, rate, share, err)
				}
				if !split.Reconciles() {
					t.Errorf("total=%v rate=%s share=%s: %v - %v - %v - %v does not reconcile",
						total, rate, share, split.SponsorTotal, split.PlatformFee,
						split.ContributorPool, split.MaintainerPool)
				}
				// And nothing may go negative, at any rate.
				if split.PlatformFee < 0 || split.ContributorPool < 0 || split.MaintainerPool < 0 {
					t.Errorf("total=%v rate=%s share=%s produced a negative component: %+v",
						total, rate, share, split)
				}
			}
		}
	}
}

// TestSplitSponsorTotal_ClampsOutOfRangeRates. A misconfigured rate must not
// invert the arithmetic - a negative fee would hand the platform's money to
// contributors, and over 100% would owe more than the sponsor put in.
func TestSplitSponsorTotal_ClampsOutOfRangeRates(t *testing.T) {
	neg, err := SplitSponsorTotal(1000, feeCfg("-10", "20"))
	if err != nil {
		t.Fatalf("SplitSponsorTotal: %v", err)
	}
	if neg.PlatformFee != 0 {
		t.Errorf("negative rate produced fee %v, want 0", neg.PlatformFee)
	}

	over, err := SplitSponsorTotal(1000, feeCfg("150", "20"))
	if err != nil {
		t.Fatalf("SplitSponsorTotal: %v", err)
	}
	if over.PlatformFee != 1000 || over.Net != 0 {
		t.Errorf("rate over 100%% gave fee=%v net=%v, want 1000 and 0", over.PlatformFee, over.Net)
	}
	if over.ContributorPool != 0 || over.MaintainerPool != 0 {
		t.Errorf("pools should be empty when the fee takes everything: %+v", over)
	}
}

// TestSplitSponsorTotal_RefusesNegativeTotal.
func TestSplitSponsorTotal_RefusesNegativeTotal(t *testing.T) {
	if _, err := SplitSponsorTotal(-1, feeCfg("5", "20")); err == nil {
		t.Error("a negative sponsor total was accepted")
	}
}

// TestSplitSponsorTotal_SnapshotsTheRates so a stored row can be re-derived
// after the configured rate has moved on.
func TestSplitSponsorTotal_SnapshotsTheRates(t *testing.T) {
	split, _ := SplitSponsorTotal(1000, feeCfg("7.5", "35"))
	if math.Abs(split.FeeRatePct-7.5) > 1e-9 || math.Abs(split.MaintainerSharePct-35) > 1e-9 {
		t.Errorf("rates = %v/%v, want 7.5/35 snapshotted", split.FeeRatePct, split.MaintainerSharePct)
	}
}
