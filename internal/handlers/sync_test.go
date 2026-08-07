package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
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
// Shared helpers for the "sync" domain test suite (sync_test.go only).
// Everything here is prefixed syncSuite to stay unique across the
// concurrently-written test files in this package.
// ---------------------------------------------------------------------------

const syncSuiteJWTSecret = "sync-suite-test-jwt-secret"

func syncSuiteConfig() config.Config {
	return config.Config{JWTSecret: syncSuiteJWTSecret}
}

// syncSuiteToken issues a signed JWT for userID/role using the sync suite's
// fixed JWT secret, matching how production issues tokens.
func syncSuiteToken(t *testing.T, userID uuid.UUID, role string) string {
	t.Helper()
	tok, err := auth.IssueJWT(syncSuiteJWTSecret, userID, role, "", "", 0)
	if err != nil {
		t.Fatalf("issue sync-suite jwt: %v", err)
	}
	return tok
}

// syncSuiteInsertUser inserts a uniquely-identified user row with the given
// role directly via SQL and returns its id.
func syncSuiteInsertUser(t *testing.T, d *db.DB, role string) uuid.UUID {
	t.Helper()
	ghID := time.Now().UnixNano()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id)
VALUES ($1, $2, $3)
RETURNING id
`, role, "sync-suite-user-"+uuid.NewString(), ghID).Scan(&id)
	if err != nil {
		t.Fatalf("insert sync-suite test user: %v", err)
	}
	return id
}

// syncSuiteInsertProject inserts a uniquely-identified, verified project
// owned by ownerID and returns its id.
func syncSuiteInsertProject(t *testing.T, d *db.DB, ownerID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified')
RETURNING id
`, ownerID, "sync-suite/"+uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("insert sync-suite test project: %v", err)
	}
	return id
}

// newSyncRoutesTestApp mounts /projects/:id/sync and /projects/:id/sync/jobs
// exactly as internal/api/api.go wires SyncHandler: both routes require
// auth.RequireAuth.
func newSyncRoutesTestApp(cfg config.Config, d *db.DB) *fiber.App {
	app := fiber.New()
	h := handlers.NewSyncHandler(d)
	app.Post("/projects/:id/sync", auth.RequireAuth(cfg.JWTSecret), h.EnqueueFullSync())
	app.Get("/projects/:id/sync/jobs", auth.RequireAuth(cfg.JWTSecret), h.JobsForProject())
	return app
}

func syncSuiteDo(t *testing.T, app *fiber.App, method, path, token string) (int, map[string]any) {
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

// syncSuiteJobRow is a minimal projection of a sync_jobs row used to verify
// EnqueueFullSync's inserts directly via SQL.
type syncSuiteJobRow struct {
	jobType string
	status  string
}

func syncSuiteReadJobs(t *testing.T, d *db.DB, projectID uuid.UUID) []syncSuiteJobRow {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(), `
SELECT job_type, status FROM sync_jobs WHERE project_id = $1 ORDER BY created_at
`, projectID)
	if err != nil {
		t.Fatalf("query sync_jobs: %v", err)
	}
	defer rows.Close()

	var out []syncSuiteJobRow
	for rows.Next() {
		var r syncSuiteJobRow
		if err := rows.Scan(&r.jobType, &r.status); err != nil {
			t.Fatalf("scan sync_jobs row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// EnqueueFullSync
// ---------------------------------------------------------------------------

func TestSyncEnqueueFullSync(t *testing.T) {
	d := testDB(t)
	app := newSyncRoutesTestApp(syncSuiteConfig(), d)

	owner := syncSuiteInsertUser(t, d, "maintainer")
	project := syncSuiteInsertProject(t, d, owner)

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := syncSuiteDo(t, app, "POST", "/projects/"+project.String()+"/sync", "")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("garbage bearer token is rejected", func(t *testing.T) {
		status, _ := syncSuiteDo(t, app, "POST", "/projects/"+project.String()+"/sync", "not-a-real-jwt")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("malformed project id is rejected", func(t *testing.T) {
		tok := syncSuiteToken(t, owner, "maintainer")
		status, body := syncSuiteDo(t, app, "POST", "/projects/not-a-uuid/sync", tok)
		if status != fiber.StatusBadRequest {
			t.Errorf("status = %d, want %d", status, fiber.StatusBadRequest)
		}
		if body["error"] != "invalid_project_id" {
			t.Errorf("error = %v, want invalid_project_id", body["error"])
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		tok := syncSuiteToken(t, owner, "maintainer")
		status, body := syncSuiteDo(t, app, "POST", "/projects/"+uuid.NewString()+"/sync", tok)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "project_not_found" {
			t.Errorf("error = %v, want project_not_found", body["error"])
		}
	})

	t.Run("authenticated non-owner non-admin is forbidden", func(t *testing.T) {
		other := syncSuiteInsertUser(t, d, "contributor")
		tok := syncSuiteToken(t, other, "contributor")
		status, body := syncSuiteDo(t, app, "POST", "/projects/"+project.String()+"/sync", tok)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
		if body["error"] != "forbidden" {
			t.Errorf("error = %v, want forbidden", body["error"])
		}
		if jobs := syncSuiteReadJobs(t, d, project); len(jobs) != 0 {
			t.Errorf("expected no jobs enqueued by a forbidden caller, got %d: %+v", len(jobs), jobs)
		}
	})

	t.Run("owner enqueues sync_issues and sync_prs jobs", func(t *testing.T) {
		tok := syncSuiteToken(t, owner, "maintainer")
		status, body := syncSuiteDo(t, app, "POST", "/projects/"+project.String()+"/sync", tok)
		if status != fiber.StatusAccepted {
			t.Fatalf("status = %d, want %d", status, fiber.StatusAccepted)
		}
		if body["queued"] != true {
			t.Errorf("queued = %v, want true", body["queued"])
		}

		jobs := syncSuiteReadJobs(t, d, project)
		if len(jobs) != 2 {
			t.Fatalf("expected 2 sync_jobs rows inserted, got %d: %+v", len(jobs), jobs)
		}
		gotTypes := map[string]bool{}
		for _, j := range jobs {
			gotTypes[j.jobType] = true
			if j.status != "pending" {
				t.Errorf("job %s status = %q, want pending", j.jobType, j.status)
			}
		}
		if !gotTypes["sync_issues"] || !gotTypes["sync_prs"] {
			t.Errorf("expected both sync_issues and sync_prs jobs, got %+v", jobs)
		}
	})

	t.Run("admin non-owner can also enqueue", func(t *testing.T) {
		project2 := syncSuiteInsertProject(t, d, owner)
		admin := syncSuiteInsertUser(t, d, "admin")
		tok := syncSuiteToken(t, admin, "admin")
		status, _ := syncSuiteDo(t, app, "POST", "/projects/"+project2.String()+"/sync", tok)
		if status != fiber.StatusAccepted {
			t.Errorf("status = %d, want %d", status, fiber.StatusAccepted)
		}
		if jobs := syncSuiteReadJobs(t, d, project2); len(jobs) != 2 {
			t.Errorf("expected 2 jobs enqueued by admin, got %d", len(jobs))
		}
	})
}

// ---------------------------------------------------------------------------
// JobsForProject
// ---------------------------------------------------------------------------

func TestSyncJobsForProject(t *testing.T) {
	d := testDB(t)
	app := newSyncRoutesTestApp(syncSuiteConfig(), d)

	owner := syncSuiteInsertUser(t, d, "maintainer")
	project := syncSuiteInsertProject(t, d, owner)
	ownerTok := syncSuiteToken(t, owner, "maintainer")

	// Seed jobs via the real enqueue endpoint so this test also exercises
	// the production insert path rather than hand-crafting sync_jobs rows.
	if status, _ := syncSuiteDo(t, app, "POST", "/projects/"+project.String()+"/sync", ownerTok); status != fiber.StatusAccepted {
		t.Fatalf("seed enqueue failed: status = %d, want %d", status, fiber.StatusAccepted)
	}

	t.Run("no auth is rejected", func(t *testing.T) {
		status, _ := syncSuiteDo(t, app, "GET", "/projects/"+project.String()+"/sync/jobs", "")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("garbage bearer token is rejected", func(t *testing.T) {
		status, _ := syncSuiteDo(t, app, "GET", "/projects/"+project.String()+"/sync/jobs", "garbage.token.value")
		if status != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", status, fiber.StatusUnauthorized)
		}
	})

	t.Run("nonexistent project returns 404", func(t *testing.T) {
		status, body := syncSuiteDo(t, app, "GET", "/projects/"+uuid.NewString()+"/sync/jobs", ownerTok)
		if status != fiber.StatusNotFound {
			t.Errorf("status = %d, want %d", status, fiber.StatusNotFound)
		}
		if body["error"] != "project_not_found" {
			t.Errorf("error = %v, want project_not_found", body["error"])
		}
	})

	t.Run("non-owner non-admin is forbidden", func(t *testing.T) {
		other := syncSuiteInsertUser(t, d, "contributor")
		tok := syncSuiteToken(t, other, "contributor")
		status, _ := syncSuiteDo(t, app, "GET", "/projects/"+project.String()+"/sync/jobs", tok)
		if status != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", status, fiber.StatusForbidden)
		}
	})

	t.Run("owner sees the previously enqueued jobs", func(t *testing.T) {
		status, body := syncSuiteDo(t, app, "GET", "/projects/"+project.String()+"/sync/jobs", ownerTok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		jobsAny, ok := body["jobs"].([]any)
		if !ok {
			t.Fatalf("jobs field missing or wrong type: %#v", body["jobs"])
		}
		if len(jobsAny) != 2 {
			t.Fatalf("expected 2 jobs, got %d: %+v", len(jobsAny), jobsAny)
		}
		gotTypes := map[string]bool{}
		for _, raw := range jobsAny {
			job, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("job entry has unexpected shape: %#v", raw)
			}
			jt, _ := job["job_type"].(string)
			gotTypes[jt] = true
			if job["status"] != "pending" {
				t.Errorf("job status = %v, want pending", job["status"])
			}
			if _, ok := job["id"].(string); !ok {
				t.Errorf("job id missing or not a string: %#v", job["id"])
			}
			if job["last_error"] != nil {
				t.Errorf("last_error = %v, want nil for a freshly-enqueued job", job["last_error"])
			}
		}
		if !gotTypes["sync_issues"] || !gotTypes["sync_prs"] {
			t.Errorf("expected both job types present, got %+v", jobsAny)
		}
	})

	t.Run("admin sees jobs for a project they don't own", func(t *testing.T) {
		admin := syncSuiteInsertUser(t, d, "admin")
		tok := syncSuiteToken(t, admin, "admin")
		status, body := syncSuiteDo(t, app, "GET", "/projects/"+project.String()+"/sync/jobs", tok)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", status, fiber.StatusOK)
		}
		jobsAny, _ := body["jobs"].([]any)
		if len(jobsAny) != 2 {
			t.Errorf("expected 2 jobs visible to admin, got %d", len(jobsAny))
		}
	})
}
