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
// comment, an unassign must have an existing assignee). See the final report
// for two related gaps this uncovered: Assign() has no guard against
// assigning an issue that's already assigned, and Reject() has no persisted
// "already rejected" state to guard against at all.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
	return app
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
	admin := projectsFxUser(t, d.Pool)
	adminToken := projectsFxJWT(t, cfg.JWTSecret, admin, "admin")

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
		// bypass at all. Assign() does allow role=="admin", same as Verify().
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), adminToken, map[string]any{"assignee": "bob"})
		// The project has no github_app_installation_id, so even an authorized
		// admin stops here rather than reaching the (untestable) network call.
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	t.Run("project with no github app installation returns 400", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "POST", fmt.Sprintf("/projects/%s/issues/1/assign", project), maintainerToken, map[string]any{"assignee": "bob"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400, body=%s", status, body)
		}
		assertErrorCode(t, body, "project_has_no_github_app_installation")
	})

	// NOTE (gap, not a test): Assign() has no check anywhere in its source for
	// "issue already has an assignee" - unlike Apply() (issue_already_assigned)
	// and Unassign() (issue_has_no_assignees). A second Assign() call for a
	// different assignee is not rejected locally at all; it would just proceed
	// to call GitHub's AddIssueAssignees again. So "Assign when already
	// assigned" cannot be asserted as a 4xx here - the guard the task
	// description assumes does not exist in the handler. Flagged in the final
	// report.
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
