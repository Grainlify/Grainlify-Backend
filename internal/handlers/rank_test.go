package handlers

// White-box tests for rank.go: GetRankTier is a pure function of an int
// position, and GetRankTierDisplayName/GetRankTierColor are pure functions
// of a RankTier value. No HTTP, no DB - package handlers (not handlers_test)
// so these can exercise the unexported... well, actually all three are
// exported, but the task specifically calls for white-box placement here.
//
// Helper/type names below are prefixed rankSuite* to stay unique within the
// package-handlers namespace shared with github_oauth_internal_test.go and
// github_webhooks_internal_test.go (the only other white-box test files).

import "testing"

func TestGetRankTier(t *testing.T) {
	cases := []struct {
		name     string
		position int
		want     RankTier
	}{
		{"large negative position", -1000, RankBronze},
		{"negative position", -1, RankBronze},
		{"zero position", 0, RankBronze},
		{"position 1 - top edge of conqueror", 1, RankConqueror},
		{"position 5 - bottom edge of conqueror", 5, RankConqueror},
		{"position 6 - top edge of ace", 6, RankAce},
		{"position 10 - bottom edge of ace", 10, RankAce},
		{"position 11 - top edge of crown", 11, RankCrown},
		{"position 20 - bottom edge of crown", 20, RankCrown},
		{"position 21 - top edge of diamond", 21, RankDiamond},
		{"position 50 - bottom edge of diamond", 50, RankDiamond},
		{"position 51 - top edge of gold", 51, RankGold},
		{"position 100 - bottom edge of gold", 100, RankGold},
		{"position 101 - top edge of silver", 101, RankSilver},
		{"position 500 - bottom edge of silver", 500, RankSilver},
		{"position 501 - just past silver falls back to bronze", 501, RankBronze},
		{"position 1000 - well below 500", 1000, RankBronze},
		{"very large position", 1 << 30, RankBronze},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GetRankTier(tc.position); got != tc.want {
				t.Errorf("GetRankTier(%d) = %q, want %q", tc.position, got, tc.want)
			}
		})
	}
}

func TestGetRankTierDisplayName(t *testing.T) {
	cases := []struct {
		name string
		tier RankTier
		want string
	}{
		{"conqueror", RankConqueror, "Conqueror"},
		{"ace", RankAce, "Ace"},
		{"crown", RankCrown, "Crown"},
		{"diamond", RankDiamond, "Diamond"},
		{"gold", RankGold, "Gold"},
		{"silver", RankSilver, "Silver"},
		{"bronze", RankBronze, "Bronze"},
		// RankTierUnranked is declared in rank.go but GetRankTier never
		// actually returns it for any position - the position<=0 branch
		// (and the final fallthrough) both return RankBronze instead (see
		// TestGetRankTier above). It's still a valid, constructible value of
		// the RankTier string type though, so its display-name/color
		// mapping is exercised here rather than silently skipped.
		{"unranked (declared but never produced by GetRankTier)", RankTierUnranked, "Unranked"},
		{"unknown/garbage tier falls back to the default branch", RankTier("totally-unknown-tier"), "Bronze"},
		{"empty-string tier falls back to the default branch", RankTier(""), "Bronze"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GetRankTierDisplayName(tc.tier); got != tc.want {
				t.Errorf("GetRankTierDisplayName(%q) = %q, want %q", tc.tier, got, tc.want)
			}
		})
	}
}

func TestGetRankTierColor(t *testing.T) {
	cases := []struct {
		name string
		tier RankTier
		want string
	}{
		{"conqueror", RankConqueror, "#FFD700"},
		{"ace", RankAce, "#FF6B6B"},
		{"crown", RankCrown, "#4ECDC4"},
		{"diamond", RankDiamond, "#95E1D3"},
		{"gold", RankGold, "#F7DC6F"},
		{"silver", RankSilver, "#C0C0C0"},
		{"bronze", RankBronze, "#CD7F32"},
		// Same dead-in-practice note as TestGetRankTierDisplayName above:
		// GetRankTier never produces RankTierUnranked, but the mapping is
		// still real, reachable code and is tested here.
		{"unranked (declared but never produced by GetRankTier)", RankTierUnranked, "#7a6b5a"},
		{"unknown/garbage tier falls back to the default branch", RankTier("totally-unknown-tier"), "#CD7F32"},
		{"empty-string tier falls back to the default branch", RankTier(""), "#CD7F32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GetRankTierColor(tc.tier); got != tc.want {
				t.Errorf("GetRankTierColor(%q) = %q, want %q", tc.tier, got, tc.want)
			}
		})
	}
}
