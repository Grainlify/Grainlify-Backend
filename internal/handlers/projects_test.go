package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// projectsTestJWTSecret is used both to mint tokens (projectsFxJWT) and to
// configure auth.RequireAuth in newProjectsTestApp/newIssueAppsTestApp, so
// tokens issued for this test slice validate.
const projectsTestJWTSecret = "grainlify-test-jwt-secret-projects-domain"

// projectsFxJWT issues a real HS256 JWT via internal/auth's own issuing
// function, matching exactly what auth.RequireAuth expects.
func projectsFxJWT(t *testing.T, secret string, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(secret, userID, role, "", "", time.Hour)
	if err != nil {
		t.Fatalf("projectsFxJWT: issue jwt: %v", err)
	}
	return tok
}

// newProjectsTestApp wires a fiber app exposing exactly the routes
// internal/api/api.go registers against handlers.ProjectsHandler, including
// the same auth.RequireAuth middleware production uses.
func newProjectsTestApp(cfg config.Config, d *db.DB) *fiber.App {
	h := handlers.NewProjectsHandler(cfg, d)
	app := fiber.New()
	app.Post("/projects", auth.RequireAuth(cfg.JWTSecret), h.Create())
	app.Get("/projects/mine", auth.RequireAuth(cfg.JWTSecret), h.Mine())
	app.Get("/projects/pending-setup", auth.RequireAuth(cfg.JWTSecret), h.PendingSetup())
	app.Put("/projects/:id/metadata", auth.RequireAuth(cfg.JWTSecret), h.UpdateMetadata())
	app.Post("/projects/:id/verify", auth.RequireAuth(cfg.JWTSecret), h.Verify())
	return app
}

func TestProjectsHandler_Create(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newProjectsTestApp(cfg, d)

	owner := projectsFxUser(t, d.Pool)
	token := projectsFxJWT(t, cfg.JWTSecret, owner, "contributor")
	_, ecoName := projectsFxEcosystem(t, d.Pool)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects", "", map[string]any{
			"github_full_name": "test-owner/test-repo",
			"ecosystem_name":   ecoName,
		})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("valid payload creates a project", func(t *testing.T) {
		fullName := fmt.Sprintf("test-owner-%s/test-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
		status, body := projectsFxDoJSON(t, app, "POST", "/projects", token, map[string]any{
			"github_full_name": fullName,
			"ecosystem_name":   ecoName,
		})
		if status != fiber.StatusCreated {
			t.Fatalf("status = %d, want 201, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["github_full_name"] != fullName {
			t.Errorf("github_full_name = %v, want %v", resp["github_full_name"], fullName)
		}
		if resp["status"] != "pending_verification" {
			t.Errorf("status = %v, want pending_verification", resp["status"])
		}
		if id, _ := resp["id"].(string); id == "" {
			t.Errorf("expected non-empty id in response, got %v", resp["id"])
		}
	})

	t.Run("missing github_full_name returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects", token, map[string]any{
			"ecosystem_name": ecoName,
		})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing ecosystem_name returns 400", func(t *testing.T) {
		fullName := fmt.Sprintf("test-owner-%s/test-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects", token, map[string]any{
			"github_full_name": fullName,
		})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("unknown ecosystem_name returns 400", func(t *testing.T) {
		fullName := fmt.Sprintf("test-owner-%s/test-repo-%s", uuid.New().String()[:8], uuid.New().String()[:8])
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects", token, map[string]any{
			"github_full_name": fullName,
			"ecosystem_name":   "no-such-ecosystem-" + uuid.New().String(),
		})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})
}

func TestProjectsHandler_Mine(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newProjectsTestApp(cfg, d)

	userA := projectsFxUser(t, d.Pool)
	userB := projectsFxUser(t, d.Pool)
	tokenA := projectsFxJWT(t, cfg.JWTSecret, userA, "contributor")

	projA := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: userA, Status: "verified", NeedsMetadata: false})
	projB := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: userB, Status: "verified", NeedsMetadata: false})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/mine", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("returns only the caller's own projects", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/mine", tokenA, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := projectsFxIDSet(items)
		if !ids[projA.String()] {
			t.Errorf("expected caller's own project %s in Mine() response", projA)
		}
		if ids[projB.String()] {
			t.Errorf("Mine() leaked another user's project %s", projB)
		}
	})

	t.Run("user with no projects gets an empty array, not null", func(t *testing.T) {
		userC := projectsFxUser(t, d.Pool)
		tokenC := projectsFxJWT(t, cfg.JWTSecret, userC, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/mine", tokenC, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if items == nil {
			t.Errorf("expected JSON [] (non-null empty array) for a user with no projects, got null")
		}
		if len(items) != 0 {
			t.Errorf("expected 0 projects for a fresh user, got %d", len(items))
		}
	})
}

func TestProjectsHandler_PendingSetup(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newProjectsTestApp(cfg, d)

	userA := projectsFxUser(t, d.Pool)
	tokenA := projectsFxJWT(t, cfg.JWTSecret, userA, "contributor")

	pendingID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: userA, Status: "verified", NeedsMetadata: true})
	doneID := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: userA, Status: "verified", NeedsMetadata: false})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "GET", "/projects/pending-setup", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("returns only projects needing metadata for the caller", func(t *testing.T) {
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/pending-setup", tokenA, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := projectsFxIDSet(items)
		if !ids[pendingID.String()] {
			t.Errorf("expected needs_metadata=true project %s in PendingSetup() response", pendingID)
		}
		if ids[doneID.String()] {
			t.Errorf("PendingSetup() should not include needs_metadata=false project %s", doneID)
		}
	})

	t.Run("does not leak another user's pending-setup projects", func(t *testing.T) {
		userB := projectsFxUser(t, d.Pool)
		tokenB := projectsFxJWT(t, cfg.JWTSecret, userB, "contributor")
		status, body := projectsFxDoJSON(t, app, "GET", "/projects/pending-setup", tokenB, nil)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := projectsFxIDSet(items)
		if ids[pendingID.String()] {
			t.Errorf("PendingSetup() leaked user A's project %s to user B", pendingID)
		}
	})
}

func TestProjectsHandler_UpdateMetadata(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newProjectsTestApp(cfg, d)

	owner := projectsFxUser(t, d.Pool)
	ownerToken := projectsFxJWT(t, cfg.JWTSecret, owner, "contributor")
	_, ecoName := projectsFxEcosystem(t, d.Pool)

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: true})
		status, _ := projectsFxDoJSON(t, app, "PUT", "/projects/"+id.String()+"/metadata", "", map[string]any{"description": "x"})
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid project id returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "PUT", "/projects/not-a-uuid/metadata", ownerToken, map[string]any{"description": "x"})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "PUT", "/projects/"+uuid.New().String()+"/metadata", ownerToken, map[string]any{"description": "x"})
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner is forbidden, including a non-owner admin", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: true})

		other := projectsFxUser(t, d.Pool)
		otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")
		status, _ := projectsFxDoJSON(t, app, "PUT", "/projects/"+id.String()+"/metadata", otherToken, map[string]any{"description": "x"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403 for non-owner", status)
		}

		// Surprise worth flagging: unlike Verify() (which allows role=="admin" to
		// bypass ownership), UpdateMetadata()'s source only checks
		// ownerUserID != userID - there is no role check at all. So a non-owner
		// admin is forbidden here too.
		admin := projectsFxUser(t, d.Pool)
		adminToken := projectsFxJWT(t, cfg.JWTSecret, admin, "admin")
		status, _ = projectsFxDoJSON(t, app, "PUT", "/projects/"+id.String()+"/metadata", adminToken, map[string]any{"description": "x"})
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403 even for an admin who isn't the owner (UpdateMetadata has no admin bypass, unlike Verify)", status)
		}
	})

	t.Run("owner can update metadata and it clears needs_metadata to false", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: true})

		var before bool
		if err := d.Pool.QueryRow(context.Background(), `SELECT needs_metadata FROM projects WHERE id=$1`, id).Scan(&before); err != nil {
			t.Fatalf("query before: %v", err)
		}
		if !before {
			t.Fatalf("precondition failed: needs_metadata should start true")
		}

		desc := "updated description " + uuid.New().String()
		status, body := projectsFxDoJSON(t, app, "PUT", "/projects/"+id.String()+"/metadata", ownerToken, map[string]any{
			"description":    desc,
			"ecosystem_name": ecoName,
			"language":       "Go",
			"category":       "tooling",
			"tags":           []string{"cli"},
		})
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", status, body)
		}

		var after bool
		var gotDesc string
		if err := d.Pool.QueryRow(context.Background(), `SELECT needs_metadata, description FROM projects WHERE id=$1`, id).Scan(&after, &gotDesc); err != nil {
			t.Fatalf("query after: %v", err)
		}
		if after {
			t.Errorf("needs_metadata = true after UpdateMetadata(), want false - this is what makes a project become visible on the public List() endpoint")
		}
		if gotDesc != desc {
			t.Errorf("description = %q, want %q", gotDesc, desc)
		}
	})

	t.Run("unknown ecosystem_name returns 400", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner, Status: "verified", NeedsMetadata: true})
		status, _ := projectsFxDoJSON(t, app, "PUT", "/projects/"+id.String()+"/metadata", ownerToken, map[string]any{
			"ecosystem_name": "no-such-ecosystem-" + uuid.New().String(),
		})
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})
}

func TestProjectsHandler_Verify(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: projectsTestJWTSecret}
	app := newProjectsTestApp(cfg, d)

	owner := projectsFxUser(t, d.Pool)
	ownerToken := projectsFxJWT(t, cfg.JWTSecret, owner, "contributor")

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner})
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects/"+id.String()+"/verify", "", nil)
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})

	t.Run("invalid project id returns 400", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects/not-a-uuid/verify", ownerToken, nil)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects/"+uuid.New().String()+"/verify", ownerToken, nil)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner})
		other := projectsFxUser(t, d.Pool)
		otherToken := projectsFxJWT(t, cfg.JWTSecret, other, "contributor")
		status, _ := projectsFxDoJSON(t, app, "POST", "/projects/"+id.String()+"/verify", otherToken, nil)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})

	t.Run("owner request is accepted (queued)", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner})
		status, body := projectsFxDoJSON(t, app, "POST", "/projects/"+id.String()+"/verify", ownerToken, nil)
		if status != fiber.StatusAccepted {
			t.Fatalf("status = %d, want 202, body=%s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp["queued"] != true {
			t.Errorf("queued = %v, want true", resp["queued"])
		}
	})

	t.Run("admin (non-owner) request is also accepted - contrast with UpdateMetadata's lack of admin bypass", func(t *testing.T) {
		id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{OwnerUserID: owner})
		admin := projectsFxUser(t, d.Pool)
		adminToken := projectsFxJWT(t, cfg.JWTSecret, admin, "admin")
		status, body := projectsFxDoJSON(t, app, "POST", "/projects/"+id.String()+"/verify", adminToken, nil)
		if status != fiber.StatusAccepted {
			t.Fatalf("status = %d, want 202, body=%s", status, body)
		}
	})
}
