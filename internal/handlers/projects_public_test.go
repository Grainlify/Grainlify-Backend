package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// ---------------------------------------------------------------------------
// Shared fixtures for the projects / projects_public / issue_applications
// test slice (this file, projects_test.go, issue_applications_test.go).
//
// Every identifier below is prefixed with "projectsFx" specifically so it
// cannot collide with fixtures other concurrently-developed *_test.go files
// in this package define for their own domains (auth-core, oauth/github-app,
// admin, webhooks).
// ---------------------------------------------------------------------------

// projectsFxGHUserBase/Seq produce unique users.github_user_id values (the
// column has a UNIQUE constraint) even when many fixtures are created in a
// tight loop within the same process.
var projectsFxGHUserBase = time.Now().UnixNano()
var projectsFxGHUserSeq int64

func projectsFxNextGHUserID() int64 {
	return projectsFxGHUserBase + atomic.AddInt64(&projectsFxGHUserSeq, 1)
}

// projectsFxUser inserts a minimal row into users and returns its id.
func projectsFxUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ('contributor', $1, $2)
RETURNING id
`, "test-user-"+uuid.New().String(), projectsFxNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("projectsFxUser: insert user: %v", err)
	}
	return id
}

// projectsFxEcosystem inserts a uniquely-named active ecosystem and returns
// (id, name). Using a fresh ecosystem per test lets List()/Recommended()
// queries be scoped with ?ecosystem= so assertions aren't polluted by rows
// concurrently-running tests insert into the shared database.
func projectsFxEcosystem(t *testing.T, pool db.DBPool) (uuid.UUID, string) {
	t.Helper()
	suffix := uuid.New().String()
	name := "Test Ecosystem " + suffix
	slug := "test-ecosystem-" + suffix
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id
`, slug, name).Scan(&id)
	if err != nil {
		t.Fatalf("projectsFxEcosystem: insert ecosystem: %v", err)
	}
	return id, name
}

// projectsFxProjectSpec configures projectsFxInsertProject. Zero values pick
// sensible defaults (see projectsFxInsertProject).
type projectsFxProjectSpec struct {
	OwnerUserID    uuid.UUID
	GitHubFullName string // defaults to a unique test-owner/test-repo pair
	EcosystemID    *uuid.UUID
	Status         string // defaults to "verified"
	NeedsMetadata  bool
	Deleted        bool
	Language       *string
	Category       *string
	Tags           []string
	Description    *string
	InstallationID *string
	StarsCount     *int
	CreatedAt      *time.Time
}

// projectsFxInsertProject inserts (or, keyed on the unique github_full_name,
// re-claims) a projects row directly via SQL per the schema in
// migrations/000001_init.up.sql plus the later ALTER TABLEs (needs_metadata,
// deleted_at, github_app_installation_id, stars_count, ecosystem_id, etc).
func projectsFxInsertProject(t *testing.T, pool db.DBPool, spec projectsFxProjectSpec) uuid.UUID {
	t.Helper()
	if spec.GitHubFullName == "" {
		spec.GitHubFullName = fmt.Sprintf("test-owner-%s/test-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
	}
	if spec.Status == "" {
		spec.Status = "verified"
	}
	var tagsJSON []byte
	if len(spec.Tags) > 0 {
		tagsJSON, _ = json.Marshal(spec.Tags)
	} else {
		tagsJSON = []byte("[]")
	}
	var deletedAt *time.Time
	if spec.Deleted {
		now := time.Now()
		deletedAt = &now
	}

	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (
  owner_user_id, github_full_name, ecosystem_id, status, needs_metadata, deleted_at,
  language, category, tags, description, github_app_installation_id, stars_count
)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (github_full_name) DO UPDATE SET
  owner_user_id = EXCLUDED.owner_user_id,
  ecosystem_id = EXCLUDED.ecosystem_id,
  status = EXCLUDED.status,
  needs_metadata = EXCLUDED.needs_metadata,
  deleted_at = EXCLUDED.deleted_at,
  language = EXCLUDED.language,
  category = EXCLUDED.category,
  tags = EXCLUDED.tags,
  description = EXCLUDED.description,
  github_app_installation_id = EXCLUDED.github_app_installation_id,
  stars_count = EXCLUDED.stars_count,
  updated_at = now()
RETURNING id
`, spec.OwnerUserID, spec.GitHubFullName, spec.EcosystemID, spec.Status, spec.NeedsMetadata, deletedAt,
		spec.Language, spec.Category, tagsJSON, spec.Description, spec.InstallationID, spec.StarsCount).Scan(&id)
	if err != nil {
		t.Fatalf("projectsFxInsertProject: insert project %q: %v", spec.GitHubFullName, err)
	}

	if spec.CreatedAt != nil {
		if _, err := pool.Exec(context.Background(), `UPDATE projects SET created_at = $2 WHERE id = $1`, id, *spec.CreatedAt); err != nil {
			t.Fatalf("projectsFxInsertProject: set created_at: %v", err)
		}
	}
	return id
}

// projectsFxSeedManyContributors inserts n github_issues rows with distinct
// author_login values for projectID, inflating its contributors_count. Used
// to make a specific project rank reliably within Recommended()'s
// contributors_count-DESC ordering despite noise from concurrently-running
// test suites sharing the same database.
func projectsFxSeedManyContributors(t *testing.T, pool db.DBPool, projectID uuid.UUID, n int) {
	t.Helper()
	base := projectsFxNextGHUserID()
	for i := 0; i < n; i++ {
		login := fmt.Sprintf("contributor-%d-%s", i, uuid.New().String()[:8])
		_, err := pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login)
VALUES ($1, $2, $3, 'open', 'seed issue', $4)
ON CONFLICT (project_id, number) DO NOTHING
`, projectID, base+int64(i), i+1, login)
		if err != nil {
			t.Fatalf("projectsFxSeedManyContributors: %v", err)
		}
	}
}

// newProjectsPublicTestApp wires a fiber app exposing exactly the routes
// internal/api/api.go registers against handlers.ProjectsPublicHandler (all
// unauthenticated, matching production).
func newProjectsPublicTestApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewProjectsPublicHandler(cfg, d)
	app := fiber.New()
	app.Get("/projects", h.List())
	app.Get("/projects/recommended", h.Recommended())
	app.Get("/projects/filters", h.FilterOptions())
	app.Get("/projects/:id", h.Get())
	app.Get("/projects/:id/issues/public", h.IssuesPublic())
	app.Get("/projects/:id/prs/public", h.PRsPublic())
	return app
}

// projectsFxDoJSON issues an HTTP request against app and returns the status
// code and raw response body. payload (if non-nil) is JSON-encoded as the
// request body; bearer (if non-empty) is sent as a Bearer Authorization
// header. A generous 20s timeout accommodates ProjectsPublicHandler.Get(),
// which makes live outbound calls to the GitHub API with no test seam.
func projectsFxDoJSON(t *testing.T, app *fiber.App, method, path, bearer string, payload any) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("projectsFxDoJSON: marshal payload: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := app.Test(req, 20000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("projectsFxDoJSON: read response body: %v", err)
	}
	return resp.StatusCode, body
}

func projectsFxIDSet(items []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if id, ok := it["id"].(string); ok {
			out[id] = true
		}
	}
	return out
}

func projectsFxContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestProjectsPublicHandler_List_NeedsMetadataFilter pins down the
// intentional-but-surprising behavior: List() (GET /projects) only shows
// projects with needs_metadata = false. A project that's verified but hasn't
// had its metadata (ecosystem/description/etc) completed by an admin or the
// owner stays invisible on the public projects list.
func TestProjectsPublicHandler_List_NeedsMetadataFilter(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	ecoID, ecoName := projectsFxEcosystem(t, d.Pool)

	visibleID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, EcosystemID: &ecoID, Status: "verified", NeedsMetadata: false,
	})
	hiddenID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, EcosystemID: &ecoID, Status: "verified", NeedsMetadata: true,
	})

	app := newProjectsPublicTestApp(config.Config{}, d)
	status, body := projectsFxDoJSON(t, app, "GET", "/projects?ecosystem="+url.QueryEscape(ecoName)+"&limit=200", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}

	var resp struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}

	ids := projectsFxIDSet(resp.Projects)
	if !ids[visibleID.String()] {
		t.Errorf("expected needs_metadata=false project %s in List() response, got %d projects: %v", visibleID, len(resp.Projects), ids)
	}
	if ids[hiddenID.String()] {
		t.Errorf("needs_metadata=true project %s appeared in List() response; List() is documented/expected to filter these out until setup is completed", hiddenID)
	}
	// Since both seeded rows share a brand-new, uniquely-named ecosystem, no
	// other concurrently-running test can contribute rows here: total must be
	// exactly 1 (the visible one).
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1 (scoped to this test's unique ecosystem)", resp.Total)
	}
}

func TestProjectsPublicHandler_List_Pagination(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	ecoID, ecoName := projectsFxEcosystem(t, d.Pool)

	base := time.Now().Add(-1 * time.Hour)
	ids := make([]uuid.UUID, 3)
	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		ids[i] = projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, EcosystemID: &ecoID, Status: "verified", NeedsMetadata: false,
			CreatedAt: &ts,
		})
	}
	// List() orders by created_at DESC, so newest (ids[2]) comes first.

	app := newProjectsPublicTestApp(config.Config{}, d)

	type page struct {
		Projects []map[string]any `json:"projects"`
		Total    int              `json:"total"`
		Limit    int              `json:"limit"`
		Offset   int              `json:"offset"`
	}

	status, body := projectsFxDoJSON(t, app, "GET", "/projects?ecosystem="+url.QueryEscape(ecoName)+"&limit=2&offset=0", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var p1 page
	if err := json.Unmarshal(body, &p1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p1.Total != 3 {
		t.Errorf("total = %d, want 3", p1.Total)
	}
	if len(p1.Projects) != 2 {
		t.Fatalf("len(projects) = %d, want 2", len(p1.Projects))
	}
	if p1.Projects[0]["id"] != ids[2].String() || p1.Projects[1]["id"] != ids[1].String() {
		t.Errorf("page 1 order = [%v, %v], want [%s, %s]", p1.Projects[0]["id"], p1.Projects[1]["id"], ids[2], ids[1])
	}

	status, body = projectsFxDoJSON(t, app, "GET", "/projects?ecosystem="+url.QueryEscape(ecoName)+"&limit=2&offset=2", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var p2 page
	if err := json.Unmarshal(body, &p2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p2.Projects) != 1 {
		t.Fatalf("len(projects) = %d, want 1", len(p2.Projects))
	}
	if p2.Projects[0]["id"] != ids[0].String() {
		t.Errorf("page 2 = %v, want [%s]", p2.Projects[0]["id"], ids[0])
	}
}

func TestProjectsPublicHandler_List_Filters(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	ecoID, ecoName := projectsFxEcosystem(t, d.Pool)

	lang := "TestLang-" + uuid.New().String()
	cat := "TestCat-" + uuid.New().String()
	tag := "test-tag-" + uuid.New().String()
	matchID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, EcosystemID: &ecoID, Status: "verified", NeedsMetadata: false,
		Language: &lang, Category: &cat, Tags: []string{tag},
	})

	otherLang := "TestLang-Other-" + uuid.New().String()
	otherCat := "TestCat-Other-" + uuid.New().String()
	otherTag := "test-tag-other-" + uuid.New().String()
	nonMatchID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, EcosystemID: &ecoID, Status: "verified", NeedsMetadata: false,
		Language: &otherLang, Category: &otherCat, Tags: []string{otherTag},
	})

	app := newProjectsPublicTestApp(config.Config{}, d)

	cases := []struct {
		name  string
		query string
	}{
		{"language", "&language=" + url.QueryEscape(lang)},
		{"category", "&category=" + url.QueryEscape(cat)},
		{"tags", "&tags=" + url.QueryEscape(tag)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := projectsFxDoJSON(t, app, "GET", "/projects?ecosystem="+url.QueryEscape(ecoName)+"&limit=200"+tc.query, "", nil)
			if status != fiber.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", status, body)
			}
			var resp struct {
				Projects []map[string]any `json:"projects"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ids := projectsFxIDSet(resp.Projects)
			if !ids[matchID.String()] {
				t.Errorf("filter %s: expected matching project %s present, got %v", tc.name, matchID, ids)
			}
			if ids[nonMatchID.String()] {
				t.Errorf("filter %s: non-matching project %s should not be present, got %v", tc.name, nonMatchID, ids)
			}
		})
	}
}

// TestProjectsPublicHandler_Recommended_NeedsMetadataFilter proves
// Recommended() (GET /projects/recommended) applies the same needs_metadata
// = false filter as List(). The "should appear" project is given a large,
// synthetic contributor count so it reliably ranks within the endpoint's
// limited result window despite unrelated rows other concurrently-running
// test suites may insert into the shared database.
func TestProjectsPublicHandler_Recommended_NeedsMetadataFilter(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)

	visibleID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false})
	hiddenID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: true})

	projectsFxSeedManyContributors(t, d.Pool, visibleID, 40)
	projectsFxSeedManyContributors(t, d.Pool, hiddenID, 40)

	app := newProjectsPublicTestApp(config.Config{}, d)
	status, body := projectsFxDoJSON(t, app, "GET", "/projects/recommended?limit=20", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := projectsFxIDSet(resp.Projects)
	if !ids[visibleID.String()] {
		t.Errorf("expected needs_metadata=false, high-contributor project %s in Recommended(), got %v", visibleID, ids)
	}
	if ids[hiddenID.String()] {
		t.Errorf("needs_metadata=true project %s appeared in Recommended() despite equally-high contributor count; exclusion should be structural (WHERE clause), not ranking-based", hiddenID)
	}
}

// TestProjectsPublicHandler_FilterOptions_NeedsMetadataFilter proves
// FilterOptions() (GET /projects/filters) sources its language/category/tag
// facets only from projects that are verified, not soft-deleted, and have
// needs_metadata = false - matching List()/Recommended().
func TestProjectsPublicHandler_FilterOptions_NeedsMetadataFilter(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)

	lang := "TestLangFO-" + uuid.New().String()
	cat := "TestCatFO-" + uuid.New().String()
	tag := "test-tag-fo-" + uuid.New().String()
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
		Language: &lang, Category: &cat, Tags: []string{tag},
	})

	hiddenLang := "TestLangFO-Hidden-" + uuid.New().String()
	hiddenCat := "TestCatFO-Hidden-" + uuid.New().String()
	hiddenTag := "test-tag-fo-hidden-" + uuid.New().String()
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, Status: "verified", NeedsMetadata: true, // excluded: needs metadata
		Language: &hiddenLang, Category: &hiddenCat, Tags: []string{hiddenTag},
	})

	deletedLang := "TestLangFO-Deleted-" + uuid.New().String()
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, Status: "verified", NeedsMetadata: false, Deleted: true, // excluded: soft-deleted
		Language: &deletedLang,
	})

	unverifiedLang := "TestLangFO-Unverified-" + uuid.New().String()
	projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, Status: "pending_verification", NeedsMetadata: false, // excluded: not verified
		Language: &unverifiedLang,
	})

	app := newProjectsPublicTestApp(config.Config{}, d)
	status, body := projectsFxDoJSON(t, app, "GET", "/projects/filters", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp struct {
		Languages  []string `json:"languages"`
		Categories []string `json:"categories"`
		Tags       []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !projectsFxContains(resp.Languages, lang) {
		t.Errorf("expected language %q present in filter options", lang)
	}
	if projectsFxContains(resp.Languages, hiddenLang) {
		t.Errorf("needs_metadata=true project's language %q leaked into filter options", hiddenLang)
	}
	if projectsFxContains(resp.Languages, deletedLang) {
		t.Errorf("soft-deleted project's language %q leaked into filter options", deletedLang)
	}
	if projectsFxContains(resp.Languages, unverifiedLang) {
		t.Errorf("unverified project's language %q leaked into filter options", unverifiedLang)
	}

	if !projectsFxContains(resp.Categories, cat) {
		t.Errorf("expected category %q present in filter options", cat)
	}
	if projectsFxContains(resp.Categories, hiddenCat) {
		t.Errorf("needs_metadata=true project's category %q leaked into filter options", hiddenCat)
	}

	if !projectsFxContains(resp.Tags, tag) {
		t.Errorf("expected tag %q present in filter options", tag)
	}
	if projectsFxContains(resp.Tags, hiddenTag) {
		t.Errorf("needs_metadata=true project's tag %q leaked into filter options", hiddenTag)
	}
}

func TestProjectsPublicHandler_Get(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	app := newProjectsPublicTestApp(config.Config{}, d)

	t.Run("invalid id format returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/not-a-uuid", "", nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent id returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/"+uuid.New().String(), "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("soft-deleted project returns 404, not the deleted row", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false, Deleted: true,
		})
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String(), "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404 for soft-deleted project", status)
		}
	})

	t.Run("unverified project returns 404", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "pending_verification", NeedsMetadata: false,
		})
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String(), "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404 for a project still pending_verification", status)
		}
	})

	t.Run("existing verified project returns 200 with expected fields", func(t *testing.T) {
		// Get() enriches the response from the live GitHub API (internal/github's
		// repo.go hardcodes https://api.github.com with no test seam - unlike
		// oauth.go/api.go which expose an overridable var for their own package's
		// tests only). octocat/Hello-World is GitHub's canonical, permanently
		// public demo repository, so this sub-test depends on outbound network
		// access to GitHub succeeding.
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
			GitHubFullName: "octocat/Hello-World",
		})
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String(), "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["id"] != id.String() {
			t.Errorf("id = %v, want %v", resp["id"], id.String())
		}
		if resp["github_full_name"] != "octocat/Hello-World" {
			t.Errorf("github_full_name = %v, want octocat/Hello-World", resp["github_full_name"])
		}
		if _, ok := resp["repo"]; !ok {
			t.Errorf("expected a \"repo\" key enriched from the live GitHub API, got keys=%v", resp)
		}
	})
}

func TestProjectsPublicHandler_IssuesPublic(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	app := newProjectsPublicTestApp(config.Config{}, d)

	id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false})
	title := "test issue title " + uuid.New().String()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login, url)
VALUES ($1, $2, 1, 'open', $3, 'someone', 'https://github.com/test-owner/test-repo/issues/1')
`, id, projectsFxNextGHUserID(), title)
	if err != nil {
		t.Fatalf("seed github_issues: %v", err)
	}

	t.Run("returns seeded issues for a verified project", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String()+"/issues/public", "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp struct {
			Issues []map[string]any `json:"issues"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, it := range resp.Issues {
			if it["title"] == title {
				found = true
			}
		}
		if !found {
			t.Errorf("expected seeded issue %q in response, got %d issues", title, len(resp.Issues))
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/"+uuid.New().String()+"/issues/public", "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("invalid project id returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/not-a-uuid/issues/public", "", nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})
}

func TestProjectsPublicHandler_PRsPublic(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	app := newProjectsPublicTestApp(config.Config{}, d)

	id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: false})
	title := "test pr title " + uuid.New().String()
	_, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, author_login, url, merged)
VALUES ($1, $2, 1, 'open', $3, 'someone', 'https://github.com/test-owner/test-repo/pull/1', false)
`, id, projectsFxNextGHUserID(), title)
	if err != nil {
		t.Fatalf("seed github_pull_requests: %v", err)
	}

	t.Run("returns seeded PRs for a verified project", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String()+"/prs/public", "", nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var resp struct {
			PRs []map[string]any `json:"prs"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		found := false
		for _, it := range resp.PRs {
			if it["title"] == title {
				found = true
			}
		}
		if !found {
			t.Errorf("expected seeded PR %q in response, got %d PRs", title, len(resp.PRs))
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/"+uuid.New().String()+"/prs/public", "", nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})
}
