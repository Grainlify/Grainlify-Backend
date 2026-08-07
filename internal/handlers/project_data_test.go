package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Shared helpers for the "project data" domain test suite (project_data_test.go
// only). Everything here is prefixed projectDataSuite to stay unique across
// the concurrently-written test files in this package.
//
// Source note (internal/handlers/project_data.go): Issues()/PRs()/Events()
// all route through the unexported projectIDForRead helper, which requires
// only that the caller be authenticated (any role) and that the project
// exist with status='verified' AND deleted_at IS NULL - it does NOT check
// project ownership (see its doc comment: "Any authenticated user can read
// project issues/PRs/events"). The other unexported helper on this handler,
// authorizeProject, is never called by Issues/PRs/Events (or by anything
// else in the repo - confirmed via a full-repo grep) - it is dead code. Both
// facts are exercised/documented by the tests below rather than assumed.
// ---------------------------------------------------------------------------

const projectDataSuiteJWTSecret = "project-data-suite-test-jwt-secret"

func projectDataSuiteConfig() config.Config {
	return config.Config{JWTSecret: projectDataSuiteJWTSecret}
}

func projectDataSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(projectDataSuiteJWTSecret, userID, role, "", "", 0)
	if err != nil {
		t.Fatalf("issue project-data-suite jwt: %v", err)
	}
	return tok
}

func projectDataSuiteInsertUser(t *testing.T, d *db.DB, role string) uuid.UUID {
	t.Helper()
	ghID := time.Now().UnixNano()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ($1, $2, $3)
RETURNING id
`, role, "project-data-suite-user-"+uuid.NewString(), ghID).Scan(&id)
	if err != nil {
		t.Fatalf("insert project-data-suite test user: %v", err)
	}
	return id
}

// projectDataSuiteInsertProject inserts a uniquely-identified project owned
// by ownerID with the given status ('verified', 'pending_verification', ...).
func projectDataSuiteInsertProject(t *testing.T, d *db.DB, ownerID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, $3)
RETURNING id
`, ownerID, "project-data-suite/"+uuid.NewString(), status).Scan(&id)
	if err != nil {
		t.Fatalf("insert project-data-suite test project: %v", err)
	}
	return id
}

func projectDataSuiteInsertIssue(t *testing.T, d *db.DB, projectID uuid.UUID, number int, title string) {
	t.Helper()
	githubIssueID := time.Now().UnixNano()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, body, author_login, url, assignees, labels, comments_count, comments)
VALUES ($1, $2, $3, 'open', $4, $5, 'octocat', $6, $7::jsonb, $8::jsonb, $9, $10::jsonb)
`, projectID, githubIssueID, number, title, "issue body for "+title,
		"https://github.com/example/repo/issues/"+strconv.Itoa(number),
		`[]`, `["bug"]`, 2, `[{"author":"reviewer","body":"lgtm"}]`)
	if err != nil {
		t.Fatalf("insert project-data-suite test issue: %v", err)
	}
}

func projectDataSuiteInsertPR(t *testing.T, d *db.DB, projectID uuid.UUID, number int, title string) {
	t.Helper()
	githubPRID := time.Now().UnixNano()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, body, author_login, url, merged)
VALUES ($1, $2, $3, 'open', $4, $5, 'octocat', $6, false)
`, projectID, githubPRID, number, title, "pr body for "+title,
		"https://github.com/example/repo/pull/"+strconv.Itoa(number))
	if err != nil {
		t.Fatalf("insert project-data-suite test pr: %v", err)
	}
}

func projectDataSuiteInsertEvent(t *testing.T, d *db.DB, projectID uuid.UUID, event, action string) string {
	t.Helper()
	deliveryID := "project-data-suite-" + uuid.NewString()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_events (delivery_id, project_id, repo_full_name, event, action, payload)
VALUES ($1, $2, 'example/repo', $3, $4, '{}'::jsonb)
`, deliveryID, projectID, event, action)
	if err != nil {
		t.Fatalf("insert project-data-suite test event: %v", err)
	}
	return deliveryID
}

// newProjectDataRoutesTestApp mounts /projects/:id/issues, /projects/:id/prs
// and /projects/:id/events exactly as internal/api/api.go wires
// ProjectDataHandler: all three require auth.RequireAuth.
func newProjectDataRoutesTestApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewProjectDataHandler(d)
	app.Get("/projects/:id/issues", auth.RequireAuth(cfg.JWTSecret), h.Issues())
	app.Get("/projects/:id/prs", auth.RequireAuth(cfg.JWTSecret), h.PRs())
	app.Get("/projects/:id/events", auth.RequireAuth(cfg.JWTSecret), h.Events())
	return app
}

func projectDataSuiteDo(t *testing.T, app *fiber.App, method, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	decoded := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err != io.EOF {
		t.Fatalf("decode response body: %v", err)
	}
	return resp.StatusCode, decoded
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func TestProjectDataIssues(t *testing.T) {
	d := testDB(t)
	app := newProjectDataRoutesTestApp(projectDataSuiteConfig(), d)

	owner := projectDataSuiteInsertUser(t, d, "maintainer")
	project := projectDataSuiteInsertProject(t, d, owner, "verified")
	projectDataSuiteInsertIssue(t, d, project, 1, "First issue")
	projectDataSuiteInsertIssue(t, d, project, 2, "Second issue")

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/issues", "")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/issues", "not-a-real-jwt")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	// BUG (internal/handlers/project_data.go:26-45, projectIDForRead):
	// every early-exit branch does
	//   return uuid.Nil, c.Status(...).JSON(fiber.Map{"error": ...})
	// but fiber's (*Ctx).JSON returns nil once the body is written
	// successfully (see gofiber/fiber/v2 ctx.go JSON: "return nil" on the
	// happy path) - so projectIDForRead's returned `error` is ALWAYS nil,
	// even on its 400/404/401/503 branches. Its callers - Issues()
	// (project_data.go:49), PRs() (:116), Events() (:166) - all do
	//   projectID, err := h.projectIDForRead(c)
	//   if err != nil { return err }
	// which never fires, so execution always falls through to the main
	// query using projectID == uuid.Nil, which matches zero rows and then
	// overwrites the response with a 200 + empty list. Net effect: a
	// malformed id, a nonexistent project, or an unverified project all
	// silently respond 200 with e.g. {"issues":null} instead of the
	// 400/404 the code appears to intend. Confirmed by direct
	// reproduction, not just by reading the source. These three subtests
	// lock in that actual (buggy) behavior; if projectIDForRead is fixed
	// to propagate a real sentinel error, they should be updated to
	// expect 400/404 instead.
	t.Run("malformed project id - BUG: silently returns 200 with an empty list instead of 400", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/not-a-uuid/issues", tok)
		if status != fiber.StatusOK {
			t.Errorf("status = %d, want %d (see BUG comment above)", status, fiber.StatusOK)
		}
		if body["issues"] != nil {
			t.Errorf("issues = %v, want nil (masked error path returns an empty list)", body["issues"])
		}
	})

	t.Run("nonexistent project - BUG: silently returns 200 with an empty list instead of 404", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+uuid.NewString()+"/issues", tok)
		if status != fiber.StatusOK {
			t.Errorf("status = %d, want %d (see BUG comment above)", status, fiber.StatusOK)
		}
		if body["issues"] != nil {
			t.Errorf("issues = %v, want nil (masked error path returns an empty list)", body["issues"])
		}
	})

	t.Run("unverified project - BUG: silently returns 200 with an empty list instead of 404", func(t *testing.T) {
		pendingOwner := projectDataSuiteInsertUser(t, d, "maintainer")
		pendingProject := projectDataSuiteInsertProject(t, d, pendingOwner, "pending_verification")
		tok := projectDataSuiteToken(t, pendingOwner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+pendingProject.String()+"/issues", tok)
		if status != fiber.StatusOK {
			t.Errorf("status = %d, want %d (see BUG comment above; a project with status != 'verified' should 404 per the exists check, but doesn't)", status, fiber.StatusOK)
		}
		if body["issues"] != nil {
			t.Errorf("issues = %v, want nil (masked error path returns an empty list)", body["issues"])
		}
	})

	t.Run("any authenticated user can read - no ownership check", func(t *testing.T) {
		stranger := projectDataSuiteInsertUser(t, d, "contributor")
		tok := projectDataSuiteToken(t, stranger, "contributor")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/issues", tok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d (a non-owner authenticated user should be able to read issues)", status, fiber.StatusOK)
		}
		issues, ok := body["issues"].([]any)
		if !ok || len(issues) != 2 {
			t.Fatalf("expected 2 issues, got %#v", body["issues"])
		}
	})

	t.Run("owner reads seeded issues with expected fields", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/issues", tok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		issuesAny, ok := body["issues"].([]any)
		if !ok {
			t.Fatalf("issues field missing or wrong type: %#v", body["issues"])
		}
		if len(issuesAny) != 2 {
			t.Fatalf("expected 2 issues, got %d", len(issuesAny))
		}
		gotTitles := map[string]bool{}
		for _, raw := range issuesAny {
			issue, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("issue entry has unexpected shape: %#v", raw)
			}
			title, _ := issue["title"].(string)
			gotTitles[title] = true
			if issue["state"] != "open" {
				t.Errorf("state = %v, want open", issue["state"])
			}
			if issue["author_login"] != "octocat" {
				t.Errorf("author_login = %v, want octocat", issue["author_login"])
			}
			if _, ok := issue["github_issue_id"]; !ok {
				t.Errorf("expected github_issue_id key present in %#v", issue)
			}
			labels, ok := issue["labels"].([]any)
			if !ok || len(labels) != 1 || labels[0] != "bug" {
				t.Errorf("labels = %#v, want [\"bug\"]", issue["labels"])
			}
			comments, ok := issue["comments"].([]any)
			if !ok || len(comments) != 1 {
				t.Errorf("comments = %#v, want 1 entry", issue["comments"])
			}
		}
		if !gotTitles["First issue"] || !gotTitles["Second issue"] {
			t.Errorf("expected both seeded issue titles present, got %+v", gotTitles)
		}
	})
}

// ---------------------------------------------------------------------------
// PRs
// ---------------------------------------------------------------------------

func TestProjectDataPRs(t *testing.T) {
	d := testDB(t)
	app := newProjectDataRoutesTestApp(projectDataSuiteConfig(), d)

	owner := projectDataSuiteInsertUser(t, d, "maintainer")
	project := projectDataSuiteInsertProject(t, d, owner, "verified")
	projectDataSuiteInsertPR(t, d, project, 7, "Fix bug")

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/prs", "")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/prs", "garbage")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	// Same projectIDForRead bug documented in detail in project_data_test.go's
	// TestProjectDataIssues ("nonexistent project" subtest) applies here too:
	// PRs() falls through to a 200 with an empty list instead of 404.
	t.Run("nonexistent project - BUG: silently returns 200 with an empty list instead of 404", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+uuid.NewString()+"/prs", tok)
		if status != fiber.StatusOK {
			t.Errorf("status = %d, want %d (see project_data.go projectIDForRead BUG)", status, fiber.StatusOK)
		}
		if body["prs"] != nil {
			t.Errorf("prs = %v, want nil (masked error path returns an empty list)", body["prs"])
		}
	})

	t.Run("authenticated non-owner reads seeded PR", func(t *testing.T) {
		stranger := projectDataSuiteInsertUser(t, d, "contributor")
		tok := projectDataSuiteToken(t, stranger, "contributor")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/prs", tok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		prsAny, ok := body["prs"].([]any)
		if !ok || len(prsAny) != 1 {
			t.Fatalf("expected 1 pr, got %#v", body["prs"])
		}
		pr, ok := prsAny[0].(map[string]any)
		if !ok {
			t.Fatalf("pr entry has unexpected shape: %#v", prsAny[0])
		}
		if pr["title"] != "Fix bug" {
			t.Errorf("title = %v, want \"Fix bug\"", pr["title"])
		}
		if pr["state"] != "open" {
			t.Errorf("state = %v, want open", pr["state"])
		}
		if pr["merged"] != false {
			t.Errorf("merged = %v, want false", pr["merged"])
		}
		numVal, ok := pr["number"].(float64)
		if !ok || int(numVal) != 7 {
			t.Errorf("number = %v, want 7", pr["number"])
		}
	})
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func TestProjectDataEvents(t *testing.T) {
	d := testDB(t)
	app := newProjectDataRoutesTestApp(projectDataSuiteConfig(), d)

	owner := projectDataSuiteInsertUser(t, d, "maintainer")
	project := projectDataSuiteInsertProject(t, d, owner, "verified")
	deliveryID := projectDataSuiteInsertEvent(t, d, project, "issues", "opened")

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/events", "")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		status, _ := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/events", "garbage")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	// Same projectIDForRead bug documented in detail in project_data_test.go's
	// TestProjectDataIssues ("nonexistent project" subtest) applies here too:
	// Events() falls through to a 200 with an empty list instead of 404.
	t.Run("nonexistent project - BUG: silently returns 200 with an empty list instead of 404", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+uuid.NewString()+"/events", tok)
		if status != fiber.StatusOK {
			t.Errorf("status = %d, want %d (see project_data.go projectIDForRead BUG)", status, fiber.StatusOK)
		}
		if body["events"] != nil {
			t.Errorf("events = %v, want nil (masked error path returns an empty list)", body["events"])
		}
	})

	t.Run("owner reads seeded event", func(t *testing.T) {
		tok := projectDataSuiteToken(t, owner, "maintainer")
		status, body := projectDataSuiteDo(t, app, "GET", "/projects/"+project.String()+"/events", tok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		eventsAny, ok := body["events"].([]any)
		if !ok || len(eventsAny) != 1 {
			t.Fatalf("expected 1 event, got %#v", body["events"])
		}
		ev, ok := eventsAny[0].(map[string]any)
		if !ok {
			t.Fatalf("event entry has unexpected shape: %#v", eventsAny[0])
		}
		if ev["delivery_id"] != deliveryID {
			t.Errorf("delivery_id = %v, want %v", ev["delivery_id"], deliveryID)
		}
		if ev["event"] != "issues" {
			t.Errorf("event = %v, want issues", ev["event"])
		}
		if ev["action"] != "opened" {
			t.Errorf("action = %v, want opened", ev["action"])
		}
	})
}
