package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// The invariant that broke: anyone the leaderboard ranks must get the same
// ranking from their public profile.
//
// TestRankingIsConsistent already asserted board == badge, but it asked
// ranking.Position directly. PublicProfile never got that far: when the login
// had no github_accounts row it returned early with a hardcoded
// {position: nil, tier_name: "Unranked", contributions_count: 0} and never
// consulted internal/ranking at all.
//
// In production that meant five of the top eight contributors had public
// profiles contradicting the board - ekwe7 sat at rank 3 with 16 merged PRs
// while /profile/public?login=ekwe7 called them unranked with zero
// contributions. The discriminator was signup, not contribution, and the
// leaderboard deliberately ranks unregistered contributors: entriesQuery LEFT
// JOINs github_accounts and falls back to COALESCE(acct.login, r.login_raw).
//
// So this test goes through the HTTP HANDLER rather than the ranking package.
// Testing the package again would have passed while the bug shipped, because
// the bug was a path that never reached the package.

func newPublicProfileRankingApp(d *db.DB) *fiber.App {
	app := fiber.New()
	app.Get("/leaderboard", handlers.NewLeaderboardHandler(d).Leaderboard())
	app.Get("/profile/public", handlers.NewUserProfileHandler(config.Config{}, d).PublicProfile())
	return app
}

// publicProfileFor fetches /profile/public?login= and decodes it. Generous
// timeout for the same reason leaderboardSuiteDoJSON uses one: the shared test
// database has accumulated many qualifying contributors.
func publicProfileFor(t *testing.T, app *fiber.App, login string) map[string]any {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/profile/public?login="+login, nil), 20000)
	if err != nil {
		t.Fatalf("app.Test(/profile/public?login=%s): %v", login, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("/profile/public?login=%s returned %d, want 200", login, resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode public profile for %s: %v", login, err)
	}
	return body
}

// An UNREGISTERED contributor - merged PRs, no github_accounts row - must get
// the same position from their public profile as the board gives them.
func TestPublicProfile_UnregisteredContributorIsNotUnranked(t *testing.T) {
	d := testDB(t)
	app := newPublicProfileRankingApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// Seeded above the single-merge tail so a bounded search can locate them:
	// absence from the board would then be a real failure rather than an
	// artefact of stopping early.
	const score = 6
	login := "lbsuite-unreg-" + uuid.New().String()[:8]
	for i := 0; i < score; i++ {
		leaderboardSuitePR(t, d.Pool, project, login)
	}
	// Deliberately NO leaderboardSuiteLinkedAccount call: this contributor has
	// never signed up, which is the whole case under test.

	boardEntry := leaderboardSuiteFindRanked(t, app, login, score)
	if boardEntry == nil {
		t.Fatalf("%q is missing from the leaderboard despite %d merged PRs", login, score)
	}
	boardRank := int(boardEntry["rank"].(float64))

	profile := publicProfileFor(t, app, login)
	rank, ok := profile["rank"].(map[string]any)
	if !ok {
		t.Fatalf("public profile for %q has no rank object: %v", login, profile)
	}

	if rank["position"] == nil {
		t.Fatalf("public profile for %q reports position=nil (%v), but the board ranks them #%d with %d merged PRs. "+
			"A contributor who has never signed up is still a contributor; the profile must ask internal/ranking "+
			"rather than returning a hardcoded Unranked.",
			login, rank["tier_name"], boardRank, score)
	}
	if got := int(rank["position"].(float64)); got != boardRank {
		t.Errorf("public profile position for %q = %d, board says #%d - these must be one ranking", login, got, boardRank)
	}
	if got := int(rank["merged_prs"].(float64)); got != score {
		t.Errorf("public profile merged_prs for %q = %d, want %d", login, got, score)
	}

	// contributions_count must be computed from author_login too - it was
	// hardcoded to 0 on the same early return.
	if got := int(profile["contributions_count"].(float64)); got != score {
		t.Errorf("public profile contributions_count for %q = %d, want %d (their %d merged PRs)",
			login, got, score, score)
	}

	// user_id stays empty: they are a contributor, not a member. That is the
	// honest answer and clients rely on it to decide what to link to.
	if got, _ := profile["user_id"].(string); got != "" {
		t.Errorf("user_id for unregistered contributor %q = %q, want \"\"", login, got)
	}
}

// The general invariant, swept across the actual board: NOBODY the leaderboard
// ranks may return a null position from /profile/public.
//
// The single-contributor test above pins the known case; this one would catch
// a different path producing the same contradiction.
func TestPublicProfile_NoRankedContributorIsUnranked(t *testing.T) {
	d := testDB(t)
	app := newPublicProfileRankingApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// Two contributors that differ ONLY in whether they signed up, so a
	// regression that reintroduces the signup discriminator fails here even if
	// the board itself is healthy.
	suffix := uuid.New().String()[:8]
	const score = 5
	unregistered := "lbsuite-sweep-anon-" + suffix
	registered := "lbsuite-sweep-member-" + suffix
	for i := 0; i < score; i++ {
		leaderboardSuitePR(t, d.Pool, project, unregistered)
		leaderboardSuitePR(t, d.Pool, project, registered)
	}
	memberID := leaderboardSuiteUser(t, d.Pool)
	leaderboardSuiteLinkedAccount(t, d.Pool, memberID, registered, "")

	status, body := leaderboardSuiteDoJSON(t, app, "/leaderboard?limit=200")
	if status != fiber.StatusOK {
		t.Fatalf("/leaderboard returned %d", status)
	}
	var entries []map[string]any
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("decode leaderboard: %v", err)
	}

	checked := 0
	for _, e := range entries {
		username, _ := e["username"].(string)
		// Only the logins this test seeded. The shared database carries rows
		// from other suites, and asserting about those would make this test
		// depend on their fixtures.
		if username != unregistered && username != registered {
			continue
		}
		checked++
		profile := publicProfileFor(t, app, username)
		rank, _ := profile["rank"].(map[string]any)
		if rank == nil || rank["position"] == nil {
			t.Errorf("%q is ranked #%v on the leaderboard but /profile/public reports no position. "+
				"Every login the board ranks must resolve to the same position on its profile.",
				username, e["rank"])
			continue
		}
		if got, want := int(rank["position"].(float64)), int(e["rank"].(float64)); got != want {
			t.Errorf("%q: profile position %d != board rank %d", username, got, want)
		}
	}
	if checked != 2 {
		t.Fatalf("expected both seeded logins on the board, found %d - the fixture did not rank as intended", checked)
	}
}
