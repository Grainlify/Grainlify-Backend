package handlers_test

// rank_determinism_test.go – tie-break determinism tests for the leaderboard.
//
// The leaderboard query uses:
//
//	ORDER BY contribution_count DESC, ac.login ASC
//
// These tests verify that:
//   1. Pure rank-tier helpers (GetRankTier, GetRankTierDisplayName,
//      GetRankTierColor) are stable and cover every tier boundary.
//   2. When multiple contributors share the same contribution count, the
//      leaderboard always returns them in ascending-login (ASCII) order —
//      consistently across repeated requests.
//   3. Pagination windows are stable when the page boundary falls inside a
//      group of tied contributors (no entry appears twice or is skipped).
//   4. Contributors with strictly higher scores are never displaced by
//      tie-break logic (score ordering is preserved).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Pure-function tests (no DB required)
// ---------------------------------------------------------------------------

func TestGetRankTierBoundaries(t *testing.T) {
	cases := []struct {
		position int
		want     handlers.RankTier
	}{
		// Invalid / zero position
		{0, handlers.RankBronze},
		{-1, handlers.RankBronze},
		// Conqueror: 1-5
		{1, handlers.RankConqueror},
		{5, handlers.RankConqueror},
		// Ace: 6-10
		{6, handlers.RankAce},
		{10, handlers.RankAce},
		// Crown: 11-20
		{11, handlers.RankCrown},
		{20, handlers.RankCrown},
		// Diamond: 21-50
		{21, handlers.RankDiamond},
		{50, handlers.RankDiamond},
		// Gold: 51-100
		{51, handlers.RankGold},
		{100, handlers.RankGold},
		// Silver: 101-500
		{101, handlers.RankSilver},
		{500, handlers.RankSilver},
		// Bronze: >500
		{501, handlers.RankBronze},
		{9999, handlers.RankBronze},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("pos=%d", tc.position), func(t *testing.T) {
			got := handlers.GetRankTier(tc.position)
			assert.Equal(t, tc.want, got,
				"GetRankTier(%d) = %q, want %q", tc.position, got, tc.want)
		})
	}
}

func TestGetRankTierDisplayNameCoversAllTiers(t *testing.T) {
	tiers := []struct {
		tier handlers.RankTier
		want string
	}{
		{handlers.RankConqueror, "Conqueror"},
		{handlers.RankAce, "Ace"},
		{handlers.RankCrown, "Crown"},
		{handlers.RankDiamond, "Diamond"},
		{handlers.RankGold, "Gold"},
		{handlers.RankSilver, "Silver"},
		{handlers.RankBronze, "Bronze"},
		{handlers.RankTierUnranked, "Unranked"},
		// Unknown tier falls back to "Bronze"
		{handlers.RankTier("unknown"), "Bronze"},
	}

	for _, tc := range tiers {
		t.Run(string(tc.tier), func(t *testing.T) {
			assert.Equal(t, tc.want, handlers.GetRankTierDisplayName(tc.tier))
		})
	}
}

func TestGetRankTierColorCoversAllTiers(t *testing.T) {
	tiers := []handlers.RankTier{
		handlers.RankConqueror,
		handlers.RankAce,
		handlers.RankCrown,
		handlers.RankDiamond,
		handlers.RankGold,
		handlers.RankSilver,
		handlers.RankBronze,
		handlers.RankTierUnranked,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			color := handlers.GetRankTierColor(tier)
			assert.NotEmpty(t, color, "GetRankTierColor(%q) must return a non-empty string", tier)
		})
	}

	// Unknown tier returns the same fallback as Bronze.
	assert.Equal(t,
		handlers.GetRankTierColor(handlers.RankBronze),
		handlers.GetRankTierColor(handlers.RankTier("unknown")),
	)
}

// ---------------------------------------------------------------------------
// Integration tests — tie-break determinism (require TEST_DB_URL)
// ---------------------------------------------------------------------------

// seedTieBreakDataset inserts contributors whose login names are deliberately
// in non-alphabetical insertion order, so that without the tie-break we would
// observe non-deterministic ordering.
//
// Layout:
//
//	"zara"  → 3 contributions  (unique high score)
//	"alice" → 2 contributions  } tied
//	"mike"  → 2 contributions  } tied
//	"bob"   → 2 contributions  } tied
//	"dave"  → 1 contribution   (unique low score)
func seedTieBreakDataset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	resetLeaderboardTables(t, pool)

	ctx := t.Context()
	suffix := uuid.NewString()

	var ownerID, ecosystemID, projectID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`,
		"tiebreak-owner-"+suffix).Scan(&ownerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`,
		"tiebreak-"+suffix, "TieBreak Ecosystem "+suffix).Scan(&ecosystemID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id)
		VALUES ($1, $2, 'verified', $3) RETURNING id`,
		ownerID, "grainlify/tiebreak-"+suffix, ecosystemID).Scan(&projectID))

	// contributions[login] = count
	contributions := map[string]int{
		"zara":  3,
		"alice": 2,
		"mike":  2,
		"bob":   2,
		"dave":  1,
	}

	issueID := time.Now().UnixNano() % 1_000_000_000
	for login, count := range contributions {
		for i := 0; i < count; i++ {
			issueID++
			require.NoError(t, pool.QueryRow(ctx, `
				INSERT INTO github_issues
					(project_id, github_issue_id, number, state, title,
					 author_login, url, created_at_github, updated_at_github)
				VALUES ($1,$2,$3,'open',$4,$5,$6,now(),now())
				RETURNING id`,
				projectID, issueID, int(issueID%1_000_000),
				fmt.Sprintf("Issue %d", issueID),
				login,
				fmt.Sprintf("https://example.test/%d", issueID),
			).Scan(new(uuid.UUID)))
		}
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`TRUNCATE github_issues, github_pull_requests, projects, ecosystems, github_accounts, wallets, users CASCADE`)
	})
}

// TestLeaderboardTieBreakOrder verifies that tied contributors are returned in
// ascending-login order on every independent request.
func TestLeaderboardTieBreakOrder(t *testing.T) {
	pool := openTestPool(t)
	seedTieBreakDataset(t, pool)
	app := newLeaderboardApp(pool)

	// Fetch the full leaderboard three times — results must be identical.
	var snapshots [3]leaderboardResponse
	for i := range snapshots {
		status, body := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")
		require.Equal(t, 200, status)
		snapshots[i] = body
	}

	first := snapshots[0].Leaderboard
	require.Len(t, first, 5)

	// Verify the three repeated fetches returned the same order.
	for i := 1; i < len(snapshots); i++ {
		require.Len(t, snapshots[i].Leaderboard, 5)
		for j, entry := range snapshots[i].Leaderboard {
			assert.Equal(t, first[j].Username, entry.Username,
				"request %d: position %d differs (got %q, want %q)",
				i+1, j+1, entry.Username, first[j].Username)
		}
	}

	// Rank 1: zara (highest score = 3, unique)
	assert.Equal(t, "zara", first[0].Username)
	assert.Equal(t, 3, first[0].Contributions)

	// Ranks 2-4: alice, bob, mike — tied at 2, must be alphabetical
	tied := first[1:4]
	assert.Equal(t, "alice", tied[0].Username, "first tied entry must be alice (alphabetically first)")
	assert.Equal(t, "bob", tied[1].Username, "second tied entry must be bob")
	assert.Equal(t, "mike", tied[2].Username, "third tied entry must be mike")
	for _, e := range tied {
		assert.Equal(t, 2, e.Contributions, "all tied entries must have contribution_count=2")
	}

	// Rank 5: dave (lowest score = 1, unique)
	assert.Equal(t, "dave", first[4].Username)
	assert.Equal(t, 1, first[4].Contributions)
}

// TestLeaderboardTieBreakRankFieldsAreStable verifies that the rank integer
// assigned to each entry is stable across requests (no reordering).
func TestLeaderboardTieBreakRankFieldsAreStable(t *testing.T) {
	pool := openTestPool(t)
	seedTieBreakDataset(t, pool)
	app := newLeaderboardApp(pool)

	_, first := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")
	_, second := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")

	require.Equal(t, len(first.Leaderboard), len(second.Leaderboard))
	for i := range first.Leaderboard {
		assert.Equal(t, first.Leaderboard[i].Rank, second.Leaderboard[i].Rank,
			"rank at position %d changed between requests", i)
		assert.Equal(t, first.Leaderboard[i].Username, second.Leaderboard[i].Username,
			"username at position %d changed between requests", i)
	}
}

// TestLeaderboardTieBreakPaginationStability verifies that paginating through
// a block of tied entries is stable: no entry is duplicated or skipped.
func TestLeaderboardTieBreakPaginationStability(t *testing.T) {
	pool := openTestPool(t)
	seedTieBreakDataset(t, pool)
	app := newLeaderboardApp(pool)

	// Collect all entries via limit=2 windows (page boundary deliberately
	// falls inside the three-way tie at contribution_count=2).
	allUsernames := make([]string, 0, 5)
	seenRanks := make(map[int]string)

	offsets := []int{0, 2, 4}
	for _, off := range offsets {
		url := fmt.Sprintf("/leaderboard?limit=2&offset=%d", off)
		status, body := getLeaderboard(t, app, url)
		require.Equal(t, 200, status, "unexpected status for offset=%d", off)
		require.Equal(t, 5, body.Total, "total must be 5 for all pages")

		for _, entry := range body.Leaderboard {
			allUsernames = append(allUsernames, entry.Username)
			prev, dup := seenRanks[entry.Rank]
			assert.False(t, dup,
				"rank %d appears at least twice (first: %q, again: %q)",
				entry.Rank, prev, entry.Username)
			seenRanks[entry.Rank] = entry.Username
		}
	}

	// All 5 contributors must be covered exactly once.
	assert.Len(t, allUsernames, 5, "paginating all pages must yield exactly 5 entries")

	expectedSet := map[string]bool{
		"zara": true, "alice": true, "mike": true, "bob": true, "dave": true,
	}
	for _, name := range allUsernames {
		assert.True(t, expectedSet[name], "unexpected username %q in leaderboard", name)
		delete(expectedSet, name)
	}
	assert.Empty(t, expectedSet, "these contributors were never returned: %v", expectedSet)
}

// TestLeaderboardTieBreakDoesNotAffectScoreOrdering verifies that a
// contributor with a strictly higher score is always ranked above all
// lower-scored contributors, regardless of login name.
//
// Scenario: "zzz" has 10 contributions (should be rank 1) but would sort last
// alphabetically among a set of tied contributors with only 1 contribution
// each. The tie-break must not pull "zzz" down.
func TestLeaderboardTieBreakDoesNotAffectScoreOrdering(t *testing.T) {
	pool := openTestPool(t)
	resetLeaderboardTables(t, pool)

	ctx := t.Context()
	suffix := uuid.NewString()

	var ownerID, ecoID, projID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`,
		"score-order-owner-"+suffix).Scan(&ownerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`,
		"score-order-"+suffix, "Score Order Ecosystem "+suffix).Scan(&ecoID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id)
		VALUES ($1,$2,'verified',$3) RETURNING id`,
		ownerID, "grainlify/score-order-"+suffix, ecoID).Scan(&projID))

	// "zzz" sorts last alphabetically but has the highest score.
	contributions := map[string]int{
		"zzz":   10, // highest score, last alphabetically
		"aaron": 1,
		"betty": 1,
		"carol": 1,
	}

	issueID := time.Now().UnixNano() % 1_000_000_000
	for login, count := range contributions {
		for i := 0; i < count; i++ {
			issueID++
			require.NoError(t, pool.QueryRow(ctx, `
				INSERT INTO github_issues
					(project_id, github_issue_id, number, state, title,
					 author_login, url, created_at_github, updated_at_github)
				VALUES ($1,$2,$3,'open',$4,$5,$6,now(),now())
				RETURNING id`,
				projID, issueID, int(issueID%1_000_000),
				fmt.Sprintf("Issue %d", issueID),
				login,
				fmt.Sprintf("https://example.test/%d", issueID),
			).Scan(new(uuid.UUID)))
		}
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`TRUNCATE github_issues, github_pull_requests, projects, ecosystems, github_accounts, wallets, users CASCADE`)
	})

	app := newLeaderboardApp(pool)
	status, body := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")
	require.Equal(t, 200, status)
	require.Len(t, body.Leaderboard, 4)

	// "zzz" must be rank 1 despite sorting last alphabetically.
	assert.Equal(t, "zzz", body.Leaderboard[0].Username,
		"highest-scored contributor must be rank 1 regardless of login name")
	assert.Equal(t, 1, body.Leaderboard[0].Rank)
	assert.Equal(t, 10, body.Leaderboard[0].Contributions)

	// The three tied contributors (score=1) follow in alphabetical order.
	assert.Equal(t, "aaron", body.Leaderboard[1].Username)
	assert.Equal(t, "betty", body.Leaderboard[2].Username)
	assert.Equal(t, "carol", body.Leaderboard[3].Username)
}

// ---------------------------------------------------------------------------
// Helper — seed a minimal pool of tied contributors via the pool returned by
// openTestPool, without polluting the leaderboard_test seeders.
// ---------------------------------------------------------------------------

func seedTiedContributors(t *testing.T, pool *pgxpool.Pool, logins []string, count int) {
	t.Helper()
	resetLeaderboardTables(t, pool)

	ctx := t.Context()
	suffix := uuid.NewString()

	var ownerID, ecoID, projID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`,
		"tied-owner-"+suffix).Scan(&ownerID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`,
		"tied-"+suffix, "Tied Ecosystem "+suffix).Scan(&ecoID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, ecosystem_id)
		VALUES ($1,$2,'verified',$3) RETURNING id`,
		ownerID, "grainlify/tied-"+suffix, ecoID).Scan(&projID))

	issueID := time.Now().UnixNano() % 1_000_000_000
	for _, login := range logins {
		for i := 0; i < count; i++ {
			issueID++
			require.NoError(t, pool.QueryRow(ctx, `
				INSERT INTO github_issues
					(project_id, github_issue_id, number, state, title,
					 author_login, url, created_at_github, updated_at_github)
				VALUES ($1,$2,$3,'open',$4,$5,$6,now(),now())
				RETURNING id`,
				projID, issueID, int(issueID%1_000_000),
				fmt.Sprintf("Issue %d", issueID),
				login,
				fmt.Sprintf("https://example.test/%d", issueID),
			).Scan(new(uuid.UUID)))
		}
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`TRUNCATE github_issues, github_pull_requests, projects, ecosystems, github_accounts, wallets, users CASCADE`)
	})
}

// TestLeaderboardAllTiedContributorsAreAlphabetical seeds N contributors all
// with the same score and asserts the returned order is strictly alphabetical.
func TestLeaderboardAllTiedContributorsAreAlphabetical(t *testing.T) {
	// Insert in reverse-alphabetical order to maximise the chance of catching
	// an unstable sort.
	logins := []string{"zeta", "omega", "lambda", "gamma", "alpha"}
	pool := openTestPool(t)
	seedTiedContributors(t, pool, logins, 3)
	app := newLeaderboardApp(pool)

	status, body := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")
	require.Equal(t, 200, status)
	require.Len(t, body.Leaderboard, 5)

	expected := []string{"alpha", "gamma", "lambda", "omega", "zeta"}
	for i, entry := range body.Leaderboard {
		assert.Equal(t, expected[i], entry.Username,
			"position %d: got %q, want %q", i+1, entry.Username, expected[i])
		assert.Equal(t, 3, entry.Contributions)
	}
}

// TestLeaderboardRepeatedRequestsAreIdentical makes the same request N times
// and asserts every response is byte-for-byte identical in terms of username
// ordering. This is the canonical regression guard against flaky shuffles.
func TestLeaderboardRepeatedRequestsAreIdentical(t *testing.T) {
	pool := openTestPool(t)
	seedTieBreakDataset(t, pool)
	app := newLeaderboardApp(pool)

	const repetitions = 5
	type entry struct {
		username string
		rank     int
	}
	var baseline []entry

	for i := 0; i < repetitions; i++ {
		status, body := getLeaderboard(t, app, "/leaderboard?limit=10&offset=0")
		require.Equal(t, 200, status)

		current := make([]entry, len(body.Leaderboard))
		for j, e := range body.Leaderboard {
			current[j] = entry{e.Username, e.Rank}
		}

		if i == 0 {
			baseline = current
			continue
		}

		require.Equal(t, len(baseline), len(current),
			"request %d returned different number of entries", i+1)
		for j := range baseline {
			assert.Equal(t, baseline[j], current[j],
				"request %d, position %d differs", i+1, j+1)
		}
	}
}

// TestLeaderboardTieBreakReq verifies the deterministic tie-break via
// raw HTTP test calls, closely mirroring how an integration-test harness
// would probe the live endpoint.
func TestLeaderboardTieBreakReq(t *testing.T) {
	pool := openTestPool(t)
	seedTieBreakDataset(t, pool)
	app := newLeaderboardApp(pool)

	req1 := httptest.NewRequest("GET", "/leaderboard?limit=10&offset=0", nil)
	req2 := httptest.NewRequest("GET", "/leaderboard?limit=10&offset=0", nil)

	resp1, err := app.Test(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()

	resp2, err := app.Test(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	var body1, body2 leaderboardResponse
	require.NoError(t, json.NewDecoder(resp1.Body).Decode(&body1))
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&body2))

	require.Equal(t, len(body1.Leaderboard), len(body2.Leaderboard))
	for i := range body1.Leaderboard {
		assert.Equal(t, body1.Leaderboard[i].Username, body2.Leaderboard[i].Username,
			"position %d differs between two identical requests", i+1)
	}
}
