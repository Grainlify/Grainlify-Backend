package handlers_test

// Scope note (read this before extending these tests):
//
// IssueApplicationsHandler has no persisted "application" entity or status
// column at all - internal/handlers/issue_applications.go confirms there is
// no issue_applications table anywhere in migrations/. What looks like a
// state machine from the outside is emergent from two JSONB columns on
// github_issues (comments, assignees) that are only ever mutated as a
// side-effect of a *successful* call to the real GitHub API - and every one
// of Apply/PostBotComment/Withdraw/Assign/Unassign/Reject calls GitHub via
// internal/github, which hardcodes "https://api.github.com/..." per-call
// with no injectable client/base-URL seam reachable from this package
// (unlike internal/github/oauth.go's tokenEndpoint or api.go's userAPIURL,
// which are unexported vars only their own package's tests can override).
//
// So the terminal "success" response (200) of every one of these six
// handlers is not reachable here without either live GitHub credentials for
// a repo we control, or a GitHub mock server this codebase doesn't provide a
// seam for. Per the task brief's guidance for exactly this situation, these
// tests instead thoroughly cover everything reachable before that external
// call: auth, input validation, ownership/authorization, and the local
// state-guards the handlers *do* implement (issue must be open, must not be
// self-authored, must not already be assigned, a withdraw must own its
// comment, an unassign must have an existing assignee).
//
// Update (migration 000032): issue_applications is now a real, persisted
// table, and Assign() gained an "already assigned" guard backed by it - both
// reachable here since the guard runs before the GitHub call. Its Mine()
// read endpoint has no GitHub dependency at all and is fully covered below.
// The five recordX write helpers that keep the table in sync with
// Apply/Assign/Reject/Withdraw/Unassign are unit-tested directly in
// issue_applications_internal_test.go, since - like everything else in this
// file's scope note - the HTTP-level success path that calls them still
// isn't reachable without live GitHub credentials.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/cryptox"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// issueAppsFxEncKey returns a fresh, valid 32-byte base64 key suitable for
// config.Config.TokenEncKeyB64 / cryptox.KeyFromB64.
func issueAppsFxEncKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

// issueAppsFxLinkedAccount inserts a github_accounts row for userID with a
// real AES-GCM-encrypted access token, so github.GetLinkedAccount succeeds
// inside the handler under test. The plaintext token value is never sent
// anywhere in these tests (every scenario here returns before the handler
// would use it to call the real GitHub API), so it does not need to be a
// real credential.
func issueAppsFxLinkedAccount(t *testing.T, pool db.DBPool, userID uuid.UUID, login string, keyB64 string) {
	t.Helper()
	key, err := cryptox.KeyFromB64(keyB64)
	if err != nil {
		t.Fatalf("issueAppsFxLinkedAccount: key: %v", err)
	}
	enc, err := cryptox.EncryptAESGCM(key, []byte("dummy-access-token-for-tests"))
	if err != nil {
		t.Fatalf("issueAppsFxLinkedAccount: encrypt: %v", err)
	}
	_, err = pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, access_token, token_type, scope)
VALUES ($1, $2, $3, $4, 'bearer', 'repo')
`, userID, projectsFxNextGHUserID(), login, enc)
	if err != nil {
		t.Fatalf("issueAppsFxLinkedAccount: insert: %v", err)
	}
}

// issueAppsFxComment is one entry of a github_issues.comments JSONB array,
// matching the shape Apply()/Withdraw() read and write.
type issueAppsFxComment struct {
	ID    int64
	Body  string
	Login string
}

// issueAppsFxIssueSpec configures issueAppsFxIssue.
type issueAppsFxIssueSpec struct {
	Number      int
	State       string // defaults to "open"
	AuthorLogin string // defaults to a unique generated login
	Assignees   []string
	Comments    []issueAppsFxComment
}

// issueAppsFxIssue inserts a github_issues row for projectID per the schema
// in migrations/000003_github_issue_pr_sync.up.sql (+ later ALTERs adding
// assignees/comments) and returns its issue number.
func issueAppsFxIssue(t *testing.T, pool db.DBPool, projectID uuid.UUID, spec issueAppsFxIssueSpec) int {
	t.Helper()
	if spec.State == "" {
		spec.State = "open"
	}
	if spec.AuthorLogin == "" {
		spec.AuthorLogin = "issue-author-" + uuid.New().String()[:8]
	}

	type assigneeJSON struct {
		Login string `json:"login"`
	}
	assignees := make([]assigneeJSON, 0, len(spec.Assignees))
	for _, a := range spec.Assignees {
		assignees = append(assignees, assigneeJSON{Login: a})
	}
	assigneesJSON, _ := json.Marshal(assignees)

	type commentUserJSON struct {
		Login string `json:"login"`
	}
	type commentJSON struct {
		ID   int64           `json:"id"`
		Body string          `json:"body"`
		User commentUserJSON `json:"user"`
	}
	comments := make([]commentJSON, 0, len(spec.Comments))
	for _, c := range spec.Comments {
		comments = append(comments, commentJSON{ID: c.ID, Body: c.Body, User: commentUserJSON{Login: c.Login}})
	}
	commentsJSON, _ := json.Marshal(comments)

	_, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login, url, assignees, comments)
VALUES ($1, $2, $3, $4, 'test issue', $5, $6, $7, $8)
`, projectID, projectsFxNextGHUserID(), spec.Number, spec.State, spec.AuthorLogin,
		fmt.Sprintf("https://github.com/test-owner/test-repo/issues/%d", spec.Number), assigneesJSON, commentsJSON)
	if err != nil {
		t.Fatalf("issueAppsFxIssue: insert: %v", err)
	}
	return spec.Number
}

// newIssueAppsTestApp wires a fiber app exposing exactly the routes
// internal/api/api.go registers against handlers.IssueApplicationsHandler,
// including the same auth.RequireAuth middleware production uses.
func newIssueAppsTestApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewIssueApplicationsHandler(cfg, d, nil)
	app := fiber.New()
	app.Post("/projects/:id/issues/:number/apply", auth.RequireAuth(cfg.JWTSecret), h.Apply())
	app.Post("/projects/:id/issues/:number/bot-comment", auth.RequireAuth(cfg.JWTSecret), h.PostBotComment())
	app.Post("/projects/:id/issues/:number/withdraw", auth.RequireAuth(cfg.JWTSecret), h.Withdraw())
	app.Post("/projects/:id/issues/:number/assign", auth.RequireAuth(cfg.JWTSecret), h.Assign())
	app.Post("/projects/:id/issues/:number/unassign", auth.RequireAuth(cfg.JWTSecret), h.Unassign())
	app.Post("/projects/:id/issues/:number/reject", auth.RequireAuth(cfg.JWTSecret), h.Reject())
	app.Get("/issue-applications/me", auth.RequireAuth(cfg.JWTSecret), h.Mine())
	return app
}

// issueAppsFxPRSpec configures issueAppsFxPR.
type issueAppsFxPRSpec struct {
	Number      int
	AuthorLogin string
	Body        string
	Merged      bool
	CreatedAt   time.Time // defaults to now()
	MergedAt    *time.Time
}

// issueAppsFxPR inserts a github_pull_requests row for projectID directly,
// per the schema in migrations/000003_github_issue_pr_sync.up.sql - Mine()'s
// PR<->issue link is a read-time regex match against pr.body, so seeding
// this table directly (rather than through the untestable webhook/sync
// ingestion paths) is the only way to exercise it in tests.
func issueAppsFxPR(t *testing.T, pool db.DBPool, projectID uuid.UUID, spec issueAppsFxPRSpec) {
	t.Helper()
	if spec.CreatedAt.IsZero() {
		spec.CreatedAt = time.Now()
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, body, author_login, url, merged, merged_at_github, created_at_github, updated_at_github)
VALUES ($1, $2, $3, 'open', 'test PR', $4, $5, $6, $7, $8, $9, $9)
`, projectID, projectsFxNextGHUserID(), spec.Number, spec.Body, spec.AuthorLogin,
		fmt.Sprintf("https://github.com/test-owner/test-repo/pull/%d", spec.Number), spec.Merged, spec.MergedAt, spec.CreatedAt)
	if err != nil {
		t.Fatalf("issueAppsFxPR: insert: %v", err)
	}
}

// issueAppsFxApplicationSpec configures issueAppsFxApplication.
type issueAppsFxApplicationSpec struct {
	UserID      uuid.UUID
	ProjectID   uuid.UUID
	IssueNumber int
	GitHubLogin string
	Status      string // defaults to "applied"
}

// issueAppsFxApplication inserts an issue_applications row directly - like
// issueAppsFxPR, this bypasses the untestable GitHub-API-dependent write
// paths (Apply/Assign/...) so Mine()'s read/bucketing logic can be tested on
// its own.
func issueAppsFxApplication(t *testing.T, pool db.DBPool, spec issueAppsFxApplicationSpec) uuid.UUID {
	t.Helper()
	if spec.Status == "" {
		spec.Status = "applied"
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO issue_applications (user_id, project_id, issue_number, github_login, status, applied_at, assigned_at)
VALUES ($1, $2, $3, $4, $5,
  CASE WHEN $5 = 'applied' THEN now() ELSE NULL END,
  CASE WHEN $5 = 'assigned' THEN now() ELSE NULL END)
RETURNING id
`, spec.UserID, spec.ProjectID, spec.IssueNumber, spec.GitHubLogin, spec.Status).Scan(&id)
	if err != nil {
		t.Fatalf("issueAppsFxApplication: insert: %v", err)
	}
	return id
}

func TestIssueApplicationsHandler_Apply(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, TokenEncKeyB64: keyB64}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	contributor := projectsFxUser(t, d.Pool)
	contributorLogin := "contributor-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, contributor, contributorLogin, keyB64)
	contributorToken := projectsFxJWT(t, cfg.JWTSecret, contributor, "contributor")

	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/apply", project), "", map[string]any{"message": "hi"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid project id returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects/not-a-uuid/issues/1/apply", contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("invalid issue number returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/abc/apply", project), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing message returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 100})
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": ""})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("message too long returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 101})
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": strings.Repeat("x", 5001)})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("github account not linked returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 102})
		unlinked := projectsFxUser(t, d.Pool)
		unlinkedToken := projectsFxJWT(t, cfg.JWTSecret, unlinked, "contributor")
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), unlinkedToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "github_not_linked")
	})

	t.Run("nonexistent issue returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/999999/apply", project), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("closed issue returns 400 issue_not_open", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 103, State: "closed"})
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "issue_not_open")
	})

	t.Run("cannot apply to own issue", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 104, AuthorLogin: contributorLogin})
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "cannot_apply_to_own_issue")
	})

	t.Run("already assigned issue returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 105, Assignees: []string{"someone-else"}})
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "issue_already_assigned")
	})

	t.Run("token encryption not configured returns 503", func(t *testing.T) {
		cfgNoKey := config.Config{JWTSecret: projectsTestJWTSecret} // TokenEncKeyB64 left empty
		appNoKey := newIssueAppsTestApp(cfgNoKey, d)
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 106})
		status, _ := projectsFxDoJSON(t, appNoKey, "POST", fmt.Sprintf("/projects/%s/issues/%d/apply", project, number), contributorToken, map[string]any{"message": "hi"})
		if status != fiber.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", status)
		}
	})
}

func TestIssueApplicationsHandler_Withdraw(t *testing.T) {
	d := testDB(t)
	keyB64 := issueAppsFxEncKey()
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, TokenEncKeyB64: keyB64}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	contributor := projectsFxUser(t, d.Pool)
	contributorLogin := "contributor-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, contributor, contributorLogin, keyB64)
	contributorToken := projectsFxJWT(t, cfg.JWTSecret, contributor, "contributor")

	other := projectsFxUser(t, d.Pool)
	otherLogin := "other-" + uuid.New().String()[:8]
	issueAppsFxLinkedAccount(t, d.Pool, other, otherLogin, keyB64)
	otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")

	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/withdraw", project), "", map[string]any{"comment_id": 1})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("missing comment_id returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 200})
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/withdraw", project, number), contributorToken, map[string]any{})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("github account not linked returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 201})
		unlinked := projectsFxUser(t, d.Pool)
		unlinkedToken := projectsFxJWT(t, cfg.JWTSecret, unlinked, "contributor")
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/withdraw", project, number), unlinkedToken, map[string]any{"comment_id": 1})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent issue returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/999999/withdraw", project), contributorToken, map[string]any{"comment_id": 1})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("withdraw when never applied (unknown comment id) returns 404", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 202}) // no comments seeded
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/withdraw", project, number), contributorToken, map[string]any{"comment_id": 999})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404, body=%s", status, body)
		}
		assertErrorCode(t, body, "comment_not_found")
	})

	t.Run("withdrawing another user's application comment is forbidden", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{
			Number:   203,
			Comments: []issueAppsFxComment{{ID: 5001, Body: "application", Login: contributorLogin}},
		})
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/withdraw", project, number), otherToken, map[string]any{"comment_id": 5001})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403, body=%s", status, body)
		}
		assertErrorCode(t, body, "you_can_only_withdraw_your_own_application")
	})
}

func TestIssueApplicationsHandler_Unassign(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, GitHubAppID: "test-app-id", GitHubAppPrivateKey: "not-a-real-key"}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	maintainerToken := projectsFxJWT(t, cfg.JWTSecret, maintainer, "contributor")
	other := projectsFxUser(t, d.Pool)
	otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")

	installID := "12345"
	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified", InstallationID: &installID})
	projectNoInstall := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/unassign", project), "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("github app not configured returns 503", func(t *testing.T) {
		cfgNoApp := config.Config{JWTSecret: projectsTestJWTSecret}
		appNoApp := newIssueAppsTestApp(cfgNoApp, d)
		status, _ := projectsFxDoJSON(t, appNoApp, "POST", fmt.Sprintf("/projects/%s/issues/1/unassign", project), maintainerToken, nil)
		if status != fiber.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", status)
		}
	})

	t.Run("nonexistent issue returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/999999/unassign", project), maintainerToken, nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 300, Assignees: []string{"someone"}})
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/unassign", project, number), otherToken, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})

	t.Run("project with no github app installation returns 400", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, projectNoInstall, issueAppsFxIssueSpec{Number: 301, Assignees: []string{"someone"}})
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/unassign", projectNoInstall, number), maintainerToken, nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	t.Run("unassign when never assigned returns 400 (invalid transition rejected)", func(t *testing.T) {
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 302}) // no assignees
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/%d/unassign", project, number), maintainerToken, nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "issue_has_no_assignees")
	})
}

func TestIssueApplicationsHandler_Assign(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, GitHubAppID: "test-app-id", GitHubAppPrivateKey: "not-a-real-key"}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	maintainerToken := projectsFxJWT(t, cfg.JWTSecret, maintainer, "contributor")
	other := projectsFxUser(t, d.Pool)
	otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")
	// projectsFxAdmin, not projectsFxUser: Assign() reads the role from the
	// users table, so an admin fixture has to be an admin there.
	admin := projectsFxAdmin(t, d.Pool)
	adminToken := projectsFxJWT(t, cfg.JWTSecret, admin, "admin")
	// Stored as a contributor, claims admin in the token - the negative
	// control below.
	impostor := projectsFxUser(t, d.Pool)
	impostorToken := projectsFxJWT(t, cfg.JWTSecret, impostor, "admin")

	// Assign(), unlike Apply()/Withdraw()/Unassign(), never joins github_issues
	// at all - it only looks up the project row - so no issue rows need to
	// exist here for any of these scenarios (see file-level doc comment).
	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"}) // no installation id

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), "", map[string]any{"assignee": "bob"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid issue number returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/abc/assign", project), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing assignee returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), maintainerToken, map[string]any{"assignee": ""})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", uuid.New()), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), otherToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})

	t.Run("admin (non-owner) passes the ownership check", func(t *testing.T) {
		// Contrast with UpdateMetadata (projects_test.go), which has no admin
		// bypass at all. Assign() does allow an admin, same as Verify() - but
		// an admin according to the users table, not according to the token.
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), adminToken, map[string]any{"assignee": "bob"})
		// The project has no github_app_installation_id, so even an authorized
		// admin stops here rather than reaching the (untestable) network call.
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	// The negative control. The subtest above and this one differ in exactly
	// one thing: whether users.role says 'admin'. The tokens are identical in
	// shape and both claim admin.
	//
	// The 400 above is what "authorised" looks like here - the request got
	// past the guard and died on a missing installation. A 403 is what
	// "refused" looks like. Asserting the error code rather than just the
	// status matters: both outcomes are 4xx, and a test that only counted
	// "not 200" would pass in both worlds.
	t.Run("a non-owner claiming admin in the token but not in the database is forbidden", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), impostorToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusForbidden {
			t.Fatalf("status = %d, want 403 - a stale or forged admin claim must not "+
				"assign an issue on a project the caller does not own; body=%s", status, body)
		}
		assertErrorCode(t, body, "forbidden")
	})

	t.Run("project with no github app installation returns 400", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	t.Run("already-assigned issue returns 400 before reaching the GitHub call", func(t *testing.T) {
		// Migration 000032 closed this gap: Assign() now checks
		// issue_applications for an existing status='assigned' row before
		// doing anything else. The guard runs before the installation-token/
		// GitHub-API calls, so this is reachable even against a project that
		// *does* have an installation id configured (unlike every other
		// scenario in this test, which deliberately uses the no-installation
		// project so it never reaches that untestable network call).
		installID := "already-assigned-guard-install"
		guardedProject := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified", InstallationID: &installID})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{
			UserID: other, ProjectID: guardedProject, IssueNumber: 400, GitHubLogin: "already-assigned-contributor", Status: "assigned",
		})

		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/400/assign", guardedProject), maintainerToken, map[string]any{"assignee": "someone-else"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "issue_already_assigned")
	})

	t.Run("assignee who never applied returns 400 before reaching the GitHub call", func(t *testing.T) {
		installID := "not-applied-guard-install"
		guardedProject := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified", InstallationID: &installID})

		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/401/assign", guardedProject), maintainerToken, map[string]any{"assignee": "never-applied"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "assignee_has_not_applied")
	})

	t.Run("assignee whose application was rejected returns 400", func(t *testing.T) {
		installID := "rejected-guard-install"
		guardedProject := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified", InstallationID: &installID})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{
			UserID: other, ProjectID: guardedProject, IssueNumber: 402, GitHubLogin: "rejected-contributor", Status: "rejected",
		})

		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/402/assign", guardedProject), maintainerToken, map[string]any{"assignee": "rejected-contributor"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "assignee_has_not_applied")
	})

	t.Run("assignee who applied passes the new eligibility check", func(t *testing.T) {
		installID := "eligible-guard-install"
		guardedProject := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified", InstallationID: &installID})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{
			UserID: other, ProjectID: guardedProject, IssueNumber: 403, GitHubLogin: "eligible-contributor", Status: "applied",
		})

		// Case-insensitive match, matching recordRejection's own LOWER() usage.
		// Whatever status comes back (the GitHub App client/token call is
		// untestable here, same as every other Assign() success-path scenario
		// in this file), it must not be the new eligibility rejection.
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/403/assign", guardedProject), maintainerToken, map[string]any{"assignee": "Eligible-Contributor"})
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, body)
		}
		if resp["error"] == "assignee_has_not_applied" {
			t.Errorf("eligible applicant was rejected by the new check: status=%d, body=%s", status, body)
		}
	})
}

func TestIssueApplicationsHandler_Reject(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, GitHubAppID: "test-app-id", GitHubAppPrivateKey: "not-a-real-key"}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	maintainerToken := projectsFxJWT(t, cfg.JWTSecret, maintainer, "contributor")
	other := projectsFxUser(t, d.Pool)
	otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")

	// Reject(), like Assign(), never looks at github_issues - it has no
	// persisted "rejected" state anywhere (see file-level doc comment).
	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/reject", project), "", map[string]any{"assignee": "bob"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid issue number returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/abc/reject", project), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing assignee returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/reject", project), maintainerToken, map[string]any{"assignee": ""})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/reject", uuid.New()), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/reject", project), otherToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})

	t.Run("project with no github app installation returns 400", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/reject", project), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	// NOTE (gap, not a test): there is no way to distinguish "this application
	// was already rejected" from source, because nothing about a rejection is
	// ever persisted - Reject() only posts a GitHub comment. Calling Reject()
	// twice in a row is not guarded against anywhere in the handler. Flagged
	// in the final report.
}

func TestIssueApplicationsHandler_PostBotComment(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret, GitHubAppID: "test-app-id", GitHubAppPrivateKey: "not-a-real-key"}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	maintainerToken := projectsFxJWT(t, cfg.JWTSecret, maintainer, "contributor")
	other := projectsFxUser(t, d.Pool)
	otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")

	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), "", map[string]any{"body": "hi"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("github app not configured returns 503", func(t *testing.T) {
		cfgNoApp := config.Config{JWTSecret: projectsTestJWTSecret}
		appNoApp := newIssueAppsTestApp(cfgNoApp, d)
		status, _ := projectsFxDoJSON(t, appNoApp, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), maintainerToken, map[string]any{"body": "hi"})
		if status != fiber.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", status)
		}
	})

	t.Run("invalid issue number returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/abc/bot-comment", project), maintainerToken, map[string]any{"body": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing body returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), maintainerToken, map[string]any{"body": ""})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("body too long returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), maintainerToken, map[string]any{"body": strings.Repeat("x", 32001)})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", uuid.New()), maintainerToken, map[string]any{"body": "hi"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), otherToken, map[string]any{"body": "hi"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})

	t.Run("project with no github app installation returns 400", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/bot-comment", project), maintainerToken, map[string]any{"body": "hi"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	// NOTE: PostBotComment()'s success path (installation present, valid
	// GitHub App private key, live comment creation) requires a real GitHub
	// App installation and a live network round trip this test suite has no
	// seam to mock - see file-level doc comment. Not covered here.
}

// mineItemsFor calls GET /issue-applications/me and returns the decoded
// issue_applications array.
func mineItemsFor(t *testing.T, app *fiber.App, token string) []map[string]any {
	t.Helper()
	status, body := projectsFxDoJSON(t, app, "GET", "/issue-applications/me", token, nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp struct {
		IssueApplications []map[string]any `json:"issue_applications"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	return resp.IssueApplications
}

// TestIssueApplicationsHandler_Mine covers GET /issue-applications/me end to
// end, including the pending_review/complete bucketing that's derived at
// read time via a regex match against github_pull_requests.body - unlike
// every other handler in this file, Mine() has no GitHub API dependency, so
// its success path is fully reachable here (see file-level doc comment).
func TestIssueApplicationsHandler_Mine(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newIssueAppsTestApp(cfg, d)

	maintainer := projectsFxUser(t, d.Pool)
	project := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: maintainer, Status: "verified"})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/issue-applications/me", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("empty list for a user with no applications", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")

		items := mineItemsFor(t, app, token)
		if len(items) != 0 {
			t.Errorf("items = %v, want empty", items)
		}
	})

	t.Run("applied-only bucket", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "applied-user-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 500, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "applied"})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 || items[0]["status"] != "applied" {
			t.Fatalf("items = %+v, want one item with status=applied", items)
		}
		if items[0]["issue_number"].(float64) != float64(number) {
			t.Errorf("issue_number = %v, want %d", items[0]["issue_number"], number)
		}
	})

	t.Run("assigned with no matching PR stays bucketed as assigned", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "assigned-user-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 501, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "assigned"})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 || items[0]["status"] != "assigned" {
			t.Fatalf("items = %+v, want one item with status=assigned", items)
		}
	})

	t.Run("a PR referencing a different issue number is not a false-positive match", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "no-false-positive-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 10, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "assigned"})
		// Body mentions #100, not #10 - the numeric-boundary guard in the
		// regex must not let "#10" match inside "#100".
		issueAppsFxPR(t, d.Pool, project, issueAppsFxPRSpec{Number: 900, AuthorLogin: login, Body: "Fixes #100"})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 || items[0]["status"] != "assigned" {
			t.Fatalf("items = %+v, want one item with status=assigned (PR must not match issue #10)", items)
		}
		if items[0]["pr_number"] != nil {
			t.Errorf("pr_number = %v, want nil (no match)", items[0]["pr_number"])
		}
	})

	t.Run("assigned with an open matching PR buckets as pending_review", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "pending-review-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 502, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "assigned"})
		issueAppsFxPR(t, d.Pool, project, issueAppsFxPRSpec{Number: 901, AuthorLogin: login, Body: fmt.Sprintf("This closes #%d for good.", number)})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 || items[0]["status"] != "pending_review" {
			t.Fatalf("items = %+v, want one item with status=pending_review", items)
		}
		if items[0]["pr_number"] == nil {
			t.Errorf("pr_number missing, want 901")
		}
	})

	t.Run("assigned with a merged matching PR buckets as complete, case-insensitively", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "complete-user-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 503, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "assigned"})
		// Uppercased keyword and uppercased author login - both comparisons
		// (LOWER(author_login) and the ~* regex) are case-insensitive.
		issueAppsFxPR(t, d.Pool, project, issueAppsFxPRSpec{Number: 902, AuthorLogin: strings.ToUpper(login), Body: fmt.Sprintf("RESOLVES #%d", number), Merged: true})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 || items[0]["status"] != "complete" {
			t.Fatalf("items = %+v, want one item with status=complete", items)
		}
	})

	t.Run("labels are surfaced as a name-only string array", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "labels-user-" + uuid.New().String()[:8]
		number := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 504, AuthorLogin: "someone-else"})
		if _, err := d.Pool.Exec(context.Background(), `UPDATE github_issues SET labels = $1 WHERE project_id = $2 AND number = $3`,
			`[{"name":"bug","color":"d73a4a"}]`, project, number); err != nil {
			t.Fatalf("seed labels: %v", err)
		}
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: number, GitHubLogin: login, Status: "applied"})

		items := mineItemsFor(t, app, token)
		if len(items) != 1 {
			t.Fatalf("items = %+v, want 1", items)
		}
		labels, _ := items[0]["labels"].([]any)
		if len(labels) != 1 || labels[0] != "bug" {
			t.Errorf("labels = %v, want [bug]", labels)
		}
	})

	t.Run("rejected and withdrawn applications are excluded from the board", func(t *testing.T) {
		user := projectsFxUser(t, d.Pool)
		token := projectsFxJWT(t, cfg.JWTSecret, user, "contributor")
		login := "excluded-user-" + uuid.New().String()[:8]
		n1 := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 505, AuthorLogin: "someone-else"})
		n2 := issueAppsFxIssue(t, d.Pool, project, issueAppsFxIssueSpec{Number: 506, AuthorLogin: "someone-else"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: n1, GitHubLogin: login, Status: "rejected"})
		issueAppsFxApplication(t, d.Pool, issueAppsFxApplicationSpec{UserID: user, ProjectID: project, IssueNumber: n2, GitHubLogin: login, Status: "withdrawn"})

		items := mineItemsFor(t, app, token)
		if len(items) != 0 {
			t.Fatalf("items = %+v, want empty (rejected/withdrawn excluded)", items)
		}
	})
}

// assertErrorCode decodes body as {"error": "..."} and fails t if it doesn't
// match want.
func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("assertErrorCode: decode response: %v (body=%s)", err, body)
	}
	if resp["error"] != want {
		t.Errorf("error = %v, want %q (body=%s)", resp["error"], want, body)
	}
}
