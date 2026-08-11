package handlers_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/jagadeesh/grainlify/backend/internal/ranking"
)

// ---------------------------------------------------------------------------
// The rules the ranking is supposed to enforce, one test each.
//
// These reuse the leaderboardSuite* fixtures from leaderboard_test.go, and
// the same discipline: the shared test database is global and other suites
// seed into it concurrently, so nothing here asserts an absolute rank or an
// exact result-set size. Each test seeds a uuid-suffixed login and asserts
// only about that login.
// ---------------------------------------------------------------------------

// A bot must never be ranked. dependabot[bot] sat at #2 on the production
// board, above every human but one, which is the first thing a new
// contributor saw.
func TestLeaderboardRules_BotsAreNotRanked(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// Seeded well above the single-merge tail so that if the bot filter
	// regressed, this login would rank high enough for a short bounded search
	// to find it - i.e. absence here is proof, not an artefact of giving up.
	const botScore = 5
	suffix := uuid.New().String()[:8]
	botLogin := "lbsuite-dependabot-" + suffix + "[bot]"
	for i := 0; i < botScore; i++ {
		leaderboardSuitePR(t, d.Pool, project, botLogin)
	}

	// A human with a similar-looking login that merely CONTAINS "[bot]"
	// mid-string must still be ranked - the filter is a suffix match, not a
	// substring one, so it cannot swallow a real contributor.
	humanLogin := "lbsuite-robot[bot]ic-" + suffix
	for i := 0; i < botScore; i++ {
		leaderboardSuitePR(t, d.Pool, project, humanLogin)
	}

	if e := leaderboardSuiteFindRanked(t, app, botLogin, botScore); e != nil {
		t.Errorf("bot %q is ranked at position %v with %v merged PRs; bots must be excluded from the leaderboard",
			botLogin, e["rank"], e["merged_prs"])
	}
	if e := leaderboardSuiteFindRanked(t, app, humanLogin, botScore); e == nil {
		t.Errorf("human %q was excluded; the bot filter must match the \"[bot]\" suffix, not the substring", humanLogin)
	}
}

// Only merged pull requests count. Under the previous definition the board
// could be topped by opening pull requests and never landing them.
func TestLeaderboardRules_OnlyMergedPullRequestsCount(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// A contributor with a large amount of unlanded activity and nothing
	// merged. Six open PRs plus six issues would have scored 12 before.
	const noiseScore = 6
	farmerLogin := "lbsuite-farmer-" + uuid.New().String()[:8]
	for i := 0; i < noiseScore; i++ {
		leaderboardSuiteOpenPR(t, d.Pool, project, farmerLogin)
		leaderboardSuiteIssue(t, d.Pool, project, farmerLogin)
	}
	leaderboardSuiteClosedUnmergedPR(t, d.Pool, project, farmerLogin)

	if e := leaderboardSuiteFindRanked(t, app, farmerLogin, 2); e != nil {
		t.Errorf("%q is ranked with %v merged PRs despite having landed nothing "+
			"(6 open PRs, 6 issues, 1 closed-unmerged PR): %v",
			farmerLogin, e["merged_prs"], e)
	}
}

// The default board is a rolling window; all-time is a secondary view.
func TestLeaderboardRules_SeasonWindowIsDefaultAndAllTimeIsOptIn(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	const score = 4
	veteranLogin := "lbsuite-veteran-" + uuid.New().String()[:8]
	// Merged comfortably outside the 90-day window.
	old := time.Now().Add(-ranking.DefaultSeasonWindow - 30*24*time.Hour)
	for i := 0; i < score; i++ {
		leaderboardSuiteMergedPRAt(t, d.Pool, project, veteranLogin, old)
	}

	if e := leaderboardSuiteFindRanked(t, app, veteranLogin, score); e != nil {
		t.Errorf("%q merged %d PRs more than %v ago but still appears on the default (season) board: %v",
			veteranLogin, score, ranking.DefaultSeasonWindow, e)
	}

	// ...and is present on the all-time board.
	found := leaderboardRulesFindIn(t, app, "/leaderboard?window=all&limit=100&offset=%d", veteranLogin, score)
	if found == nil {
		t.Errorf("%q is missing from the all-time board despite %d merged PRs; "+
			"all-time must remain available as a secondary view", veteranLogin, score)
	} else if c, _ := found["merged_prs"].(float64); int(c) != score {
		t.Errorf("all-time merged_prs = %v, want %d", found["merged_prs"], score)
	}
}

// The ecosystem filter must actually filter. It was previously accepted by
// the client, sent on the query string, and ignored by the handler.
func TestLeaderboardRules_EcosystemFilterActuallyFilters(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	owner := leaderboardSuiteUser(t, d.Pool)
	ecoA, _ := leaderboardSuiteEcosystem(t, d.Pool)
	ecoB, _ := leaderboardSuiteEcosystem(t, d.Pool)
	projectA := leaderboardSuiteProject(t, d.Pool, owner, ecoA, "verified")
	projectB := leaderboardSuiteProject(t, d.Pool, owner, ecoB, "verified")

	var slugA, slugB string
	if err := d.Pool.QueryRow(t.Context(), `SELECT slug FROM ecosystems WHERE id = $1`, ecoA).Scan(&slugA); err != nil {
		t.Fatalf("read ecosystem A slug: %v", err)
	}
	if err := d.Pool.QueryRow(t.Context(), `SELECT slug FROM ecosystems WHERE id = $1`, ecoB).Scan(&slugB); err != nil {
		t.Fatalf("read ecosystem B slug: %v", err)
	}

	const score = 4
	suffix := uuid.New().String()[:8]
	loginA := "lbsuite-ecoa-" + suffix
	loginB := "lbsuite-ecob-" + suffix
	for i := 0; i < score; i++ {
		leaderboardSuitePR(t, d.Pool, projectA, loginA)
		leaderboardSuitePR(t, d.Pool, projectB, loginB)
	}

	pathA := "/leaderboard?ecosystem=" + slugA + "&limit=100&offset=%d"
	if e := leaderboardRulesFindIn(t, app, pathA, loginA, score); e == nil {
		t.Errorf("%q is missing from the %s-filtered board despite %d merged PRs there", loginA, slugA, score)
	}
	if e := leaderboardRulesFindIn(t, app, pathA, loginB, score); e != nil {
		t.Errorf("%q contributed only to ecosystem %s but appears on the %s-filtered board: %v",
			loginB, slugB, slugA, e)
	}
}

// leaderboardRulesFindIn walks a parameterised leaderboard path (which must
// contain a single %d for the offset) looking for username, applying the same
// proof-of-absence rule as leaderboardSuiteFindRanked: nil means conclusively
// absent, and an inconclusive walk is a hard failure.
func leaderboardRulesFindIn(t *testing.T, app *fiber.App, pathFmt, username string, minScore int) map[string]any {
	t.Helper()
	for page := 0; page < leaderboardSuiteMaxSearchPages; page++ {
		offset := page * 100
		status, body := leaderboardSuiteDoJSON(t, app, fmt.Sprintf(pathFmt, offset))
		if status != fiber.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200, body=%s", fmt.Sprintf(pathFmt, offset), status, body)
		}
		var entries []map[string]any
		if err := json.Unmarshal(body, &entries); err != nil {
			t.Fatalf("decode page at offset %d: %v", offset, err)
		}
		if e := leaderboardSuiteFind(entries, username); e != nil {
			return e
		}
		if len(entries) < 100 {
			return nil
		}
		if last, ok := entries[len(entries)-1]["merged_prs"].(float64); ok && int(last) < minScore {
			return nil
		}
	}
	t.Fatalf("inconclusive: scanned %d pages of %s without reaching contributors scoring below %d",
		leaderboardSuiteMaxSearchPages, pathFmt, minScore)
	return nil
}

// ---------------------------------------------------------------------------
// The consistency guarantee that internal/ranking exists to provide.
// ---------------------------------------------------------------------------

// The public leaderboard, the profile badge and the org badge must agree.
//
// Before internal/ranking there were four separate implementations of "who is
// ranked where", and they disagreed in ways nobody could see from any single
// page: the profile badge matched logins case-sensitively where the board
// grouped case-insensitively, and it ranked only contributors who had signed
// up, so an unregistered contributor ahead of you was invisible to it and
// every badge below them read one place too good.
//
// This asserts the property directly rather than asserting that the SQL is
// shared, because sharing the SQL is the current means, not the requirement.
func TestRankingIsConsistent_LeaderboardProfileAndOrgAgree(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)
	ctx := t.Context()

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// A contributor whose login is recorded with two spellings, seeded high
	// enough to be locatable. The case variance is the point: it is what the
	// old profile query got wrong.
	const score = 6
	suffix := uuid.New().String()[:8]
	lower := "lbsuite-consist-" + suffix
	upper := "LBSuite-Consist-" + suffix
	for i := 0; i < score/2; i++ {
		leaderboardSuitePR(t, d.Pool, project, lower)
		leaderboardSuitePR(t, d.Pool, project, upper)
	}

	// What the public board says.
	boardEntry := leaderboardSuiteFindRanked(t, app, lower, score)
	if boardEntry == nil {
		t.Fatalf("%q is missing from the leaderboard despite %d merged PRs", lower, score)
	}
	boardRank := int(boardEntry["rank"].(float64))
	boardScore := int(boardEntry["merged_prs"].(float64))

	if boardScore != score {
		t.Errorf("board merged_prs = %d, want %d (both spellings summed)", boardScore, score)
	}

	// What the profile badge is computed from - the same call both profile
	// endpoints make. Asked with each spelling, because a case-sensitive
	// implementation would return a different answer for each.
	for _, spelling := range []string{lower, upper} {
		pos, merged, err := ranking.Position(ctx, d.Pool, spelling, ranking.Options{}, time.Now())
		if err != nil {
			t.Fatalf("ranking.Position(%q): %v", spelling, err)
		}
		if pos == nil {
			t.Fatalf("ranking.Position(%q) returned no position, but the contributor is ranked #%d on the board",
				spelling, boardRank)
		}
		if *pos != boardRank {
			t.Errorf("profile badge position for %q = %d, but the leaderboard puts them at #%d - "+
				"the badge and the board must be the same ranking", spelling, *pos, boardRank)
		}
		if merged != boardScore {
			t.Errorf("profile badge merged_prs for %q = %d, board says %d", spelling, merged, boardScore)
		}
		// And the tier shown alongside it must match the tier the board
		// derived from the same position.
		badgeTier := handlers.GetRankTier(*pos)
		boardTier := handlers.GetRankTier(boardRank)
		if badgeTier != boardTier {
			t.Errorf("tier for %q = %v, board tier = %v", spelling, badgeTier, boardTier)
		}
		if boardEntry["rank_tier"] != string(boardTier) {
			t.Errorf("board rank_tier = %v, want %v", boardEntry["rank_tier"], boardTier)
		}
	}

	// The org side of the same guarantee: an org's position on the project
	// board and the rank_position on its own profile are one ranking.
	var orgLogin string
	if err := d.Pool.QueryRow(ctx,
		`SELECT SPLIT_PART(github_full_name, '/', 1) FROM projects WHERE id = $1`, project,
	).Scan(&orgLogin); err != nil {
		t.Fatalf("read org login: %v", err)
	}

	orgs, err := ranking.Orgs(ctx, d.Pool, ranking.Options{}, time.Now())
	if err != nil {
		t.Fatalf("ranking.Orgs: %v", err)
	}
	var wantOrgRank int
	for _, o := range orgs {
		if o.OrgLogin == orgLogin {
			wantOrgRank = o.Rank
			break
		}
	}
	if wantOrgRank == 0 {
		t.Fatalf("org %q is absent from the project ranking despite %d merged PRs in its verified repo", orgLogin, score)
	}

	gotOrgRank, _, err := ranking.OrgPosition(ctx, d.Pool, orgLogin, ranking.Options{}, time.Now())
	if err != nil {
		t.Fatalf("ranking.OrgPosition: %v", err)
	}
	if gotOrgRank == nil {
		t.Fatalf("org badge has no position for %q, but the project board ranks it #%d", orgLogin, wantOrgRank)
	}
	if *gotOrgRank != wantOrgRank {
		t.Errorf("org badge position = %d, project board position = %d - these must be one ranking",
			*gotOrgRank, wantOrgRank)
	}
}
