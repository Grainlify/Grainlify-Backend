package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Fixtures for LeaderboardHandler (internal/handlers/leaderboard.go).
//
// Every identifier here is prefixed with "leaderboardSuite" so it can't
// collide with fixtures other concurrently-developed *_test.go files in this
// package define for their own domains.
//
// Leaderboard() has no ecosystem/project scoping query param - it ranks
// EVERY contributor globally across every verified project in the shared
// test database. Other concurrently-running test suites may add their own
// verified projects/contributors at any time, so these tests avoid
// asserting on absolute rank position or exact result-set membership;
// instead they locate their own uuid-suffixed logins by name within a
// generously-sized page and assert relative/self-consistent properties
// (counts, relative ordering, rank<->tier consistency). The one place
// absolute values ARE safe to assert is the "rank" field for a given
// offset, because rank is computed purely as offset+index within the
// returned page (see leaderboard.go's `rank := offset + 1` loop), not by
// comparison with other contributors' scores.
// ---------------------------------------------------------------------------

// leaderboardSuiteNextGHUserID returns a fresh value for a BIGINT UNIQUE
// github_user_id column (users.github_user_id, github_accounts.github_user_id).
// It's randomized rather than derived from a per-file "base :=
// time.Now().UnixNano()" plus an incrementing counter: this test binary
// links many other *_test.go files in this package that each define their
// own analogous base var, and since all package-level var initializers run
// within microseconds of each other at process startup, those bases were
// observed in practice to land on the identical nanosecond - producing
// systematically colliding github_user_id values across files/tests
// ("duplicate key value violates unique constraint users_github_user_id_key").
// rand's top-level functions are auto-seeded and safe for concurrent use,
// and the ~63-bit random space makes collisions with anything else
// (including other files' own IDs, whatever scheme they use) negligible.
func leaderboardSuiteNextGHUserID() int64 {
	return rand.Int63()
}

// leaderboardSuiteItemSeq mints unique github_issues/github_pull_requests
// (github_issue_id/github_pr_id, number) values; uniqueness is only
// required per-project, but a single global counter trivially satisfies that.
var leaderboardSuiteItemSeq int64

// leaderboardSuiteUser inserts a minimal row into users and returns its id.
func leaderboardSuiteUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ('contributor', $1, $2)
RETURNING id
`, "lbsuite-user-"+uuid.New().String(), leaderboardSuiteNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("leaderboardSuiteUser: insert user: %v", err)
	}
	return id
}

// leaderboardSuiteEcosystem inserts a uniquely-named active ecosystem and
// returns (id, name).
func leaderboardSuiteEcosystem(t *testing.T, pool db.DBPool) (uuid.UUID, string) {
	t.Helper()
	suffix := uuid.New().String()
	name := "LBSuite Ecosystem " + suffix
	slug := "lbsuite-ecosystem-" + suffix
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id
`, slug, name).Scan(&id)
	if err != nil {
		t.Fatalf("leaderboardSuiteEcosystem: insert ecosystem: %v", err)
	}
	return id, name
}

// leaderboardSuiteProject inserts a projects row and returns its id. status
// defaults to "verified" when empty.
func leaderboardSuiteProject(t *testing.T, pool db.DBPool, ownerID, ecosystemID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	if status == "" {
		status = "verified"
	}
	fullName := fmt.Sprintf("lbsuite-owner-%s/lbsuite-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, ecosystem_id, status)
VALUES ($1, $2, $3, $4)
RETURNING id
`, ownerID, fullName, ecosystemID, status).Scan(&id)
	if err != nil {
		t.Fatalf("leaderboardSuiteProject: insert project: %v", err)
	}
	return id
}

// leaderboardSuiteIssue inserts an open github_issues row authored by
// authorLogin against projectID.
func leaderboardSuiteIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin string) {
	t.Helper()
	n := atomic.AddInt64(&leaderboardSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, author_login)
VALUES ($1, $2, $3, 'open', $4)
`, projectID, n, n, authorLogin)
	if err != nil {
		t.Fatalf("leaderboardSuiteIssue: insert issue: %v", err)
	}
}

// leaderboardSuitePR inserts an open github_pull_requests row authored by
// authorLogin against projectID.
func leaderboardSuitePR(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin string) {
	t.Helper()
	n := atomic.AddInt64(&leaderboardSuiteItemSeq, 1)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login)
VALUES ($1, $2, $3, 'open', $4)
`, projectID, n, n, authorLogin)
	if err != nil {
		t.Fatalf("leaderboardSuitePR: insert PR: %v", err)
	}
}

// leaderboardSuiteLinkedAccount inserts a github_accounts row so the
// leaderboard query's LEFT JOIN resolves avatar_url/user_id for login. The
// access token value is never used by Leaderboard() (it only reads
// avatar_url/login), so a dummy byte string is fine.
func leaderboardSuiteLinkedAccount(t *testing.T, pool db.DBPool, userID uuid.UUID, login, avatarURL string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, avatar_url, access_token, token_type, scope)
VALUES ($1, $2, $3, $4, $5, 'bearer', 'repo')
`, userID, leaderboardSuiteNextGHUserID(), login, avatarURL, []byte("lbsuite-dummy-token"))
	if err != nil {
		t.Fatalf("leaderboardSuiteLinkedAccount: insert github_accounts: %v", err)
	}
}

// newLeaderboardSuiteApp wires a fiber app exposing exactly the route
// internal/api/api.go registers against handlers.LeaderboardHandler
// (unauthenticated, matching production - GET /leaderboard is registered
// outside any auth-guarded group in api.go).
func newLeaderboardSuiteApp(d *db.DB) *fiber.App {
	h := handlers.NewLeaderboardHandler(d)
	app := fiber.New()
	app.Get("/leaderboard", h.Leaderboard())
	return app
}

// leaderboardSuiteDoJSON issues a GET request against app and returns the
// status code and raw response body. Leaderboard()'s query does several
// correlated subqueries per contributor row, and this shared test database
// has accumulated hundreds of qualifying contributors across concurrent
// suites/prior runs, so a generous 20s timeout (matching the convention
// already used in projects_public_test.go for its own slow-query cases)
// avoids flaking under load instead of fiber's 1000ms test default.
func leaderboardSuiteDoJSON(t *testing.T, app *fiber.App, path string) (int, []byte) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), 20000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// leaderboardSuiteFind returns the entry for username within entries, or nil.
func leaderboardSuiteFind(entries []map[string]any, username string) map[string]any {
	for _, e := range entries {
		if e["username"] == username {
			return e
		}
	}
	return nil
}

// leaderboardSuiteFindAcrossPages searches successive limit=100 pages of GET
// /leaderboard for username, returning its entry (or nil if it isn't found
// before a page comes back short, meaning results are exhausted, or
// maxRows is reached). This suite's test database is long-lived and shared
// with every other concurrently-developed test file plus prior runs of
// these very tests (nothing here ever truncates rows), so a contributor
// with a realistic, modest contribution count is not guaranteed to land on
// the single largest page (limit is capped at 100 by the handler) - it has
// to be searched for.
func leaderboardSuiteFindAcrossPages(t *testing.T, app *fiber.App, username string, maxRows int) map[string]any {
	t.Helper()
	for offset := 0; offset < maxRows; offset += 100 {
		status, body := leaderboardSuiteDoJSON(t, app, fmt.Sprintf("/leaderboard?limit=100&offset=%d", offset))
		if status != fiber.StatusOK {
			t.Fatalf("GET /leaderboard?limit=100&offset=%d: status = %d, want 200, body=%s", offset, status, body)
		}
		var page []map[string]any
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode page at offset %d: %v", offset, err)
		}
		if e := leaderboardSuiteFind(page, username); e != nil {
			return e
		}
		if len(page) < 100 {
			return nil // short page: results are exhausted, nothing further to scan
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Leaderboard() - GET /leaderboard
// ---------------------------------------------------------------------------

func TestLeaderboardSuite_RanksByContributionCountAndReportsExpectedFields(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, ecoName := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")

	// alice: signed up (has a linked github_accounts row) and has 4
	// contributions (2 issues + 2 PRs) - the handler sums both.
	aliceUser := leaderboardSuiteUser(t, d.Pool)
	aliceLogin := "lbsuite-alice-" + uuid.New().String()[:8]
	leaderboardSuiteLinkedAccount(t, d.Pool, aliceUser, aliceLogin, "https://cdn.example/alice.png")
	leaderboardSuiteIssue(t, d.Pool, project, aliceLogin)
	leaderboardSuiteIssue(t, d.Pool, project, aliceLogin)
	leaderboardSuitePR(t, d.Pool, project, aliceLogin)
	leaderboardSuitePR(t, d.Pool, project, aliceLogin)

	// bob: never signed up (no github_accounts row), 1 contribution.
	bobLogin := "lbsuite-bob-" + uuid.New().String()[:8]
	leaderboardSuiteIssue(t, d.Pool, project, bobLogin)

	// Leaderboard() ranks every qualifying contributor globally with no
	// per-test scoping, and this database accumulates rows across every
	// test run (nothing truncates it), so alice/bob - with a modest 4 and 1
	// contributions respectively - aren't guaranteed to land on the very
	// first 100-row page. Search successive pages instead of assuming page 1.
	const maxSearchRows = 20000
	aliceEntry := leaderboardSuiteFindAcrossPages(t, app, aliceLogin, maxSearchRows)
	bobEntry := leaderboardSuiteFindAcrossPages(t, app, bobLogin, maxSearchRows)
	if aliceEntry == nil {
		t.Fatalf("alice (%s) not found among the first %d leaderboard entries", aliceLogin, maxSearchRows)
	}
	if bobEntry == nil {
		t.Fatalf("bob (%s) not found among the first %d leaderboard entries", bobLogin, maxSearchRows)
	}

	if c, _ := aliceEntry["contributions"].(float64); c != 4 {
		t.Errorf("alice contributions = %v, want 4 (2 issues + 2 PRs)", aliceEntry["contributions"])
	}
	if c, _ := bobEntry["contributions"].(float64); c != 1 {
		t.Errorf("bob contributions = %v, want 1", bobEntry["contributions"])
	}

	aliceRank, _ := aliceEntry["rank"].(float64)
	bobRank, _ := bobEntry["rank"].(float64)
	if !(aliceRank < bobRank) {
		t.Errorf("alice rank %v should be numerically less than (better than) bob rank %v, since alice has more contributions (4 vs 1)", aliceRank, bobRank)
	}

	// rank_tier/rank_tier_name must be self-consistent with the exported
	// GetRankTier/GetRankTierDisplayName functions for whichever rank the
	// handler actually assigned. We deliberately don't assume a specific
	// absolute rank (concurrent suites may seed higher-ranked contributors),
	// only that the tier matches whatever rank came back.
	wantAliceTier := handlers.GetRankTier(int(aliceRank))
	if aliceEntry["rank_tier"] != string(wantAliceTier) {
		t.Errorf("alice rank_tier = %v, want %v for rank %v", aliceEntry["rank_tier"], wantAliceTier, aliceRank)
	}
	if aliceEntry["rank_tier_name"] != handlers.GetRankTierDisplayName(wantAliceTier) {
		t.Errorf("alice rank_tier_name = %v, want %v", aliceEntry["rank_tier_name"], handlers.GetRankTierDisplayName(wantAliceTier))
	}

	if aliceEntry["avatar"] != "https://cdn.example/alice.png" {
		t.Errorf("alice avatar = %v, want the linked github_accounts.avatar_url", aliceEntry["avatar"])
	}
	if aliceEntry["user_id"] != aliceUser.String() {
		t.Errorf("alice user_id = %v, want %v", aliceEntry["user_id"], aliceUser.String())
	}

	wantBobAvatar := fmt.Sprintf("https://github.com/%s.png?size=200", bobLogin)
	if bobEntry["avatar"] != wantBobAvatar {
		t.Errorf("bob avatar = %v, want fallback %v (bob never linked a github_accounts row)", bobEntry["avatar"], wantBobAvatar)
	}
	if bobEntry["user_id"] != "" {
		t.Errorf("bob user_id = %v, want \"\" (no linked account)", bobEntry["user_id"])
	}

	ecosystems, _ := aliceEntry["ecosystems"].([]any)
	var foundEco bool
	for _, e := range ecosystems {
		if e == ecoName {
			foundEco = true
		}
	}
	if !foundEco {
		t.Errorf("alice ecosystems = %v, want it to include %q", ecosystems, ecoName)
	}
}

func TestLeaderboardSuite_ExcludesContributorsFromNonVerifiedProjects(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	pendingProject := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "pending_verification")

	login := "lbsuite-pending-" + uuid.New().String()[:8]
	leaderboardSuiteIssue(t, d.Pool, pendingProject, login)

	// This contributor has only 1 (structurally-excluded) contribution, so -
	// same reasoning as the ranking test above - a single page isn't a
	// rigorous enough check for absence; a real regression that stopped
	// filtering non-verified projects could still leave this login off page
	// 1 by rank alone, producing a false pass. Search across pages instead.
	const maxSearchRows = 20000
	if e := leaderboardSuiteFindAcrossPages(t, app, login, maxSearchRows); e != nil {
		t.Errorf("contributor %q from a pending_verification project leaked into the leaderboard: %v", login, e)
	}
}

func TestLeaderboardSuite_OffsetControlsPageAndRankNumbering(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)
	project := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")
	// Guarantee at least 2 globally-qualifying contributors exist so
	// offset=1 always has a row to return, regardless of what else is in
	// the shared database.
	leaderboardSuiteIssue(t, d.Pool, project, "lbsuite-page-a-"+uuid.New().String()[:8])
	leaderboardSuiteIssue(t, d.Pool, project, "lbsuite-page-b-"+uuid.New().String()[:8])

	status0, body0 := leaderboardSuiteDoJSON(t, app, "/leaderboard?limit=1&offset=0")
	if status0 != fiber.StatusOK {
		t.Fatalf("offset=0: status = %d, want 200, body=%s", status0, body0)
	}
	status1, body1 := leaderboardSuiteDoJSON(t, app, "/leaderboard?limit=1&offset=1")
	if status1 != fiber.StatusOK {
		t.Fatalf("offset=1: status = %d, want 200, body=%s", status1, body1)
	}

	var page0, page1 []map[string]any
	if err := json.Unmarshal(body0, &page0); err != nil {
		t.Fatalf("decode page0: %v", err)
	}
	if err := json.Unmarshal(body1, &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(page0) != 1 {
		t.Fatalf("offset=0&limit=1: got %d entries, want exactly 1", len(page0))
	}
	if len(page1) != 1 {
		t.Fatalf("offset=1&limit=1: got %d entries, want exactly 1", len(page1))
	}

	// rank is computed as offset+1 for a page's first row (see leaderboard.go:
	// `rank := offset + 1`), so this is deterministic regardless of how many
	// other contributors exist in the shared test database.
	if r, _ := page0[0]["rank"].(float64); r != 1 {
		t.Errorf("offset=0 first entry rank = %v, want 1", page0[0]["rank"])
	}
	if r, _ := page1[0]["rank"].(float64); r != 2 {
		t.Errorf("offset=1 first entry rank = %v, want 2", page1[0]["rank"])
	}
	if page0[0]["username"] == page1[0]["username"] {
		t.Errorf("offset=0 and offset=1 returned the same contributor %v; offset should page through distinct rows", page0[0]["username"])
	}
}

func TestLeaderboardSuite_OffsetBeyondTotal_ReturnsEmptyJSONArray(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	// An offset this large is far beyond any plausible number of qualifying
	// contributors in a test database, so this deterministically exercises
	// the "no rows returned" path.
	status, body := leaderboardSuiteDoJSON(t, app, "/leaderboard?limit=10&offset=5000000")
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var entries []map[string]any
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if entries == nil {
		t.Error("decoded entries is nil; handler should always return a JSON array, even when empty")
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries at an offset far beyond any plausible contributor count, want 0", len(entries))
	}
	if string(body) != "[]" {
		t.Errorf("body = %s, want the literal empty JSON array []  (not null)", body)
	}
}

// ---------------------------------------------------------------------------
// db_not_configured guard (needs no live DB).
// ---------------------------------------------------------------------------

func TestLeaderboardSuite_NilDBPool_ReturnsServiceUnavailable(t *testing.T) {
	h := handlers.NewLeaderboardHandler(&db.DB{Pool: nil})
	app := fiber.New()
	app.Get("/leaderboard", h.Leaderboard())

	status, body := leaderboardSuiteDoJSON(t, app, "/leaderboard")
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503, body=%s", status, body)
	}
}
