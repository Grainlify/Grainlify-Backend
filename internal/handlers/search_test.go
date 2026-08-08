package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Fixtures for SearchHandler (internal/handlers/search.go).
//
// Every seeded string embeds a fresh uuid so it cannot collide with rows
// other concurrently-developed test files (or prior runs) leave behind in
// this shared test database - Search()'s ILIKE filter then matches ONLY
// this test's own rows, sidestepping the "search across pages" dance
// leaderboard_test.go needs (its handler has no query filter at all).
// ---------------------------------------------------------------------------

func searchSuiteNextGHUserID() int64 { return rand.Int63() }

func searchSuiteUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ('contributor', $1, $2)
RETURNING id
`, "searchsuite-user-"+uuid.New().String(), searchSuiteNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("searchSuiteUser: insert user: %v", err)
	}
	return id
}

func searchSuiteEcosystem(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	suffix := uuid.New().String()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id
`, "searchsuite-eco-"+suffix, "SearchSuite Eco "+suffix).Scan(&id)
	if err != nil {
		t.Fatalf("searchSuiteEcosystem: insert ecosystem: %v", err)
	}
	return id
}

// searchSuiteProject inserts a projects row. When needsMetadata is false the
// row is eligible for search (mirrors Browse's own visibility rule); pass
// true, a non-"verified" status, or set deleted to exercise exclusion.
func searchSuiteProject(t *testing.T, pool db.DBPool, ownerID, ecosystemID uuid.UUID, fullName, description, status string, needsMetadata, deleted bool) uuid.UUID {
	t.Helper()
	if status == "" {
		status = "verified"
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, ecosystem_id, status, description, needs_metadata, stars_count)
VALUES ($1, $2, $3, $4, $5, $6, 1)
RETURNING id
`, ownerID, fullName, ecosystemID, status, description, needsMetadata).Scan(&id)
	if err != nil {
		t.Fatalf("searchSuiteProject: insert project: %v", err)
	}
	if deleted {
		_, err := pool.Exec(context.Background(), `UPDATE projects SET deleted_at = now() WHERE id = $1`, id)
		if err != nil {
			t.Fatalf("searchSuiteProject: soft-delete: %v", err)
		}
	}
	return id
}

var searchSuiteItemSeq int64

func searchSuiteIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, title, state, authorLogin string) uuid.UUID {
	t.Helper()
	searchSuiteItemSeq++
	n := searchSuiteItemSeq*1000 + rand.Int63n(999)
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id
`, projectID, n, n, state, title, authorLogin).Scan(&id)
	if err != nil {
		t.Fatalf("searchSuiteIssue: insert issue: %v", err)
	}
	return id
}

func searchSuitePR(t *testing.T, pool db.DBPool, projectID uuid.UUID, authorLogin string) {
	t.Helper()
	searchSuiteItemSeq++
	n := searchSuiteItemSeq*1000 + rand.Int63n(999)
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, author_login)
VALUES ($1, $2, $3, 'open', $4)
`, projectID, n, n, authorLogin)
	if err != nil {
		t.Fatalf("searchSuitePR: insert PR: %v", err)
	}
}

func searchSuiteLinkedAccount(t *testing.T, pool db.DBPool, userID uuid.UUID, login, avatarURL string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, avatar_url, access_token, token_type, scope)
VALUES ($1, $2, $3, $4, $5, 'bearer', 'repo')
`, userID, searchSuiteNextGHUserID(), login, avatarURL, []byte("searchsuite-dummy-token"))
	if err != nil {
		t.Fatalf("searchSuiteLinkedAccount: insert github_accounts: %v", err)
	}
}

func newSearchSuiteApp(d *db.DB) *fiber.App {
	h := handlers.NewSearchHandler(d)
	app := fiber.New()
	app.Get("/search", h.Search())
	return app
}

func searchSuiteDoJSON(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
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
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	return resp.StatusCode, out
}

func searchSuiteFindByID(items []any, id string) map[string]any {
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if ok && m["id"] == id {
			return m
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Search() - GET /search
// ---------------------------------------------------------------------------

func TestSearchSuite_ShortQuery_ReturnsEmptyResultsWithoutErroring(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	for _, q := range []string{"", "a"} {
		status, out := searchSuiteDoJSON(t, app, "/search?q="+q)
		if status != fiber.StatusOK {
			t.Fatalf("q=%q: status = %d, want 200", q, status)
		}
		for _, key := range []string{"projects", "issues", "contributors"} {
			arr, ok := out[key].([]any)
			if !ok {
				t.Fatalf("q=%q: %s is not an array: %v", q, key, out[key])
			}
			if len(arr) != 0 {
				t.Errorf("q=%q: %s = %v, want empty for a sub-2-character query", q, key, arr)
			}
		}
	}
}

func TestSearchSuite_MatchesProjectByNameAndByDescription(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	owner := searchSuiteUser(t, d.Pool)
	eco := searchSuiteEcosystem(t, d.Pool)
	token := "searchsuite-" + uuid.New().String()[:8]

	byName := searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/"+token+"-repo", "an unrelated description", "verified", false, false)
	descToken := "searchsuite-" + uuid.New().String()[:8]
	byDescFullName := "searchsuite-org/unrelated-name-" + uuid.New().String()[:8]
	byDesc := searchSuiteProject(t, d.Pool, owner, eco, byDescFullName, "a project about "+descToken, "verified", false, false)

	status, out := searchSuiteDoJSON(t, app, "/search?q="+token)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	projects, _ := out["projects"].([]any)
	if searchSuiteFindByID(projects, byName.String()) == nil {
		t.Errorf("searching %q did not find project matched by github_full_name; got %v", token, projects)
	}

	status2, out2 := searchSuiteDoJSON(t, app, "/search?q="+descToken)
	if status2 != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status2)
	}
	projects2, _ := out2["projects"].([]any)
	found := searchSuiteFindByID(projects2, byDesc.String())
	if found == nil {
		t.Fatalf("searching %q did not find project matched by description; got %v", descToken, projects2)
	}
	if found["github_full_name"] != byDescFullName {
		t.Errorf("github_full_name = %v, want %v", found["github_full_name"], byDescFullName)
	}
}

func TestSearchSuite_ExcludesProjectsNotVisibleOnBrowse(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	owner := searchSuiteUser(t, d.Pool)
	eco := searchSuiteEcosystem(t, d.Pool)

	pendingToken := "searchsuite-" + uuid.New().String()[:8]
	searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/"+pendingToken, "", "verified", true, false) // needs_metadata=true

	deletedToken := "searchsuite-" + uuid.New().String()[:8]
	searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/"+deletedToken, "", "verified", false, true) // soft-deleted

	unverifiedToken := "searchsuite-" + uuid.New().String()[:8]
	searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/"+unverifiedToken, "", "pending_verification", false, false)

	for _, token := range []string{pendingToken, deletedToken, unverifiedToken} {
		_, out := searchSuiteDoJSON(t, app, "/search?q="+token)
		projects, _ := out["projects"].([]any)
		if len(projects) != 0 {
			t.Errorf("token %q: expected this project to be excluded from search (not Browse-visible), got %v", token, projects)
		}
	}
}

func TestSearchSuite_MatchesOpenIssueTitle_ExcludesClosedAndInvisibleProject(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	owner := searchSuiteUser(t, d.Pool)
	eco := searchSuiteEcosystem(t, d.Pool)
	visible := searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/issue-host-"+uuid.New().String()[:8], "", "verified", false, false)
	hidden := searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/hidden-host-"+uuid.New().String()[:8], "", "verified", true, false)

	openToken := "searchsuite-issue-" + uuid.New().String()[:8]
	openIssue := searchSuiteIssue(t, d.Pool, visible, "Fix the "+openToken+" bug", "open", "searchsuite-author")

	closedToken := "searchsuite-issue-" + uuid.New().String()[:8]
	searchSuiteIssue(t, d.Pool, visible, "Fix the "+closedToken+" bug", "closed", "searchsuite-author")

	hiddenToken := "searchsuite-issue-" + uuid.New().String()[:8]
	searchSuiteIssue(t, d.Pool, hidden, "Fix the "+hiddenToken+" bug", "open", "searchsuite-author")

	status, out := searchSuiteDoJSON(t, app, "/search?q="+openToken)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	issues, _ := out["issues"].([]any)
	found := searchSuiteFindByID(issues, openIssue.String())
	if found == nil {
		t.Fatalf("expected to find the open issue matching %q; got %v", openToken, issues)
	}
	if found["project_id"] != visible.String() {
		t.Errorf("project_id = %v, want %v", found["project_id"], visible.String())
	}

	_, closedOut := searchSuiteDoJSON(t, app, "/search?q="+closedToken)
	if closedIssues, _ := closedOut["issues"].([]any); len(closedIssues) != 0 {
		t.Errorf("closed issue leaked into search results: %v", closedIssues)
	}

	_, hiddenOut := searchSuiteDoJSON(t, app, "/search?q="+hiddenToken)
	if hiddenIssues, _ := hiddenOut["issues"].([]any); len(hiddenIssues) != 0 {
		t.Errorf("issue on a not-yet-visible (needs_metadata) project leaked into search results: %v", hiddenIssues)
	}
}

func TestSearchSuite_MatchesContributorLogin_SumsIssuesAndPRs_CaseInsensitive(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	owner := searchSuiteUser(t, d.Pool)
	eco := searchSuiteEcosystem(t, d.Pool)
	project := searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/contrib-host-"+uuid.New().String()[:8], "", "verified", false, false)

	contribUser := searchSuiteUser(t, d.Pool)
	login := "SearchSuiteAlice" + uuid.New().String()[:8]
	searchSuiteLinkedAccount(t, d.Pool, contribUser, login, "https://cdn.example/alice.png")
	searchSuiteIssue(t, d.Pool, project, "unrelated title", "open", login)
	searchSuiteIssue(t, d.Pool, project, "unrelated title", "closed", login)
	searchSuitePR(t, d.Pool, project, login)

	// Search with a different case than the stored login to confirm ILIKE
	// case-insensitivity end to end.
	status, out := searchSuiteDoJSON(t, app, "/search?q="+lower(login[:12]))
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	contributors, _ := out["contributors"].([]any)
	var found map[string]any
	for _, raw := range contributors {
		m, ok := raw.(map[string]any)
		if ok && m["login"] == login {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("expected to find contributor %q; got %v", login, contributors)
	}
	if c, _ := found["contributions"].(float64); c != 3 {
		t.Errorf("contributions = %v, want 3 (2 issues + 1 PR, regardless of issue state)", found["contributions"])
	}
	if found["avatar_url"] != "https://cdn.example/alice.png" {
		t.Errorf("avatar_url = %v, want the linked github_accounts.avatar_url", found["avatar_url"])
	}
	if found["user_id"] != contribUser.String() {
		t.Errorf("user_id = %v, want %v", found["user_id"], contribUser.String())
	}
}

func TestSearchSuite_ContributorWithoutLinkedAccount_FallsBackToGitHubAvatar(t *testing.T) {
	d := testDB(t)
	app := newSearchSuiteApp(d)

	owner := searchSuiteUser(t, d.Pool)
	eco := searchSuiteEcosystem(t, d.Pool)
	project := searchSuiteProject(t, d.Pool, owner, eco, "searchsuite-org/noaccount-host-"+uuid.New().String()[:8], "", "verified", false, false)

	login := "searchsuitebob" + uuid.New().String()[:8]
	searchSuiteIssue(t, d.Pool, project, "unrelated title", "open", login)

	_, out := searchSuiteDoJSON(t, app, "/search?q="+login)
	contributors, _ := out["contributors"].([]any)
	var found map[string]any
	for _, raw := range contributors {
		m, ok := raw.(map[string]any)
		if ok && m["login"] == login {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("expected to find contributor %q; got %v", login, contributors)
	}
	wantAvatar := fmt.Sprintf("https://github.com/%s.png?size=200", login)
	if found["avatar_url"] != wantAvatar {
		t.Errorf("avatar_url = %v, want fallback %v", found["avatar_url"], wantAvatar)
	}
	if found["user_id"] != "" {
		t.Errorf("user_id = %v, want \"\" (no linked account)", found["user_id"])
	}
}

// ---------------------------------------------------------------------------
// db_not_configured guard (needs no live DB).
// ---------------------------------------------------------------------------

func TestSearchSuite_NilDBPool_ReturnsServiceUnavailable(t *testing.T) {
	h := handlers.NewSearchHandler(&db.DB{Pool: nil})
	app := fiber.New()
	app.Get("/search", h.Search())

	status, _ := searchSuiteDoJSON(t, app, "/search?q=anything")
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
