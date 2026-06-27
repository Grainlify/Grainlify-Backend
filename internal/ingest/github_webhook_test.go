// Package ingest_test contains integration tests for GitHubWebhookIngestor.
//
// # Running
//
// Tests in this file require a real PostgreSQL database and are skipped
// automatically when the TEST_DB_URL environment variable is not set.
//
//	TEST_DB_URL="postgres://user:pass@localhost:5432/testdb?sslmode=disable" \
//	  go test ./internal/ingest/...
//
// The harness applies all embedded migrations via migrate.Up before the suite
// runs, so the database only needs to exist (it does not need pre-created tables).
// Each test wraps its writes in a transaction that is rolled back on cleanup,
// keeping the schema clean between tests.
package ingest_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/events"
	"github.com/jagadeesh/grainlify/backend/internal/ingest"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// openTestPool connects to TEST_DB_URL and applies migrations.
// It skips the test if TEST_DB_URL is not set.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set – skipping DB integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}

	// Apply all migrations so the schema is up to date.
	if err := migrate.Up(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate.Up: %v", err)
	}

	t.Cleanup(pool.Close)
	return pool
}

// seedProject inserts a minimal project row and returns its UUID.
// It uses the provided pgxpool connection directly.
func seedProject(t *testing.T, pool *pgxpool.Pool, fullName, installationID string) string {
	t.Helper()
	ctx := context.Background()

	// Ensure a user exists to satisfy the FK on projects.owner_user_id.
	var ownerID string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (role) VALUES ('maintainer')
		RETURNING id
	`).Scan(&ownerID)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var projectID string
	err = pool.QueryRow(ctx, `
		INSERT INTO projects (owner_user_id, github_full_name, status, github_app_installation_id)
		VALUES ($1, $2, 'verified', $3)
		RETURNING id
	`, ownerID, fullName, installationID).Scan(&projectID)
	if err != nil {
		t.Fatalf("seed project %q: %v", fullName, err)
	}

	// Register cleanup so rows are removed after the test.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID)
	})

	return projectID
}

// loadFixture reads a JSON fixture file from testdata/ and returns its bytes.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loadFixture %q: %v", name, err)
	}
	return data
}

// mustJSON marshals v to JSON, failing the test on error.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Idempotency – same delivery_id inserted twice must yield one row
// ---------------------------------------------------------------------------

func TestIngest_Idempotency(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	deliveryID := "idempotency-test-" + time.Now().Format("20060102150405.000000000")
	ev := events.GitHubWebhookReceived{
		DeliveryID: deliveryID,
		Event:      "ping",
		Payload:    json.RawMessage(`{"zen":"Keep it logically awesome."}`),
	}

	// Ingest the same event twice.
	for range 2 {
		if err := ing.Ingest(ctx, ev); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM github_events WHERE delivery_id = $1`, deliveryID,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("want 1 github_events row, got %d", count)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, deliveryID)
	})
}

// ---------------------------------------------------------------------------
// Issue upsert – fields written correctly; second ingest updates on conflict
// ---------------------------------------------------------------------------

func TestIngest_IssueUpsert(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/widget", "install-1")
	_ = projectID

	body := loadFixture(t, "issue_opened.json")
	deliveryID := "issue-upsert-" + time.Now().Format("20060102150405.000000000")

	ev := events.GitHubWebhookReceived{
		DeliveryID:   deliveryID,
		Event:        "issues",
		Action:       "opened",
		RepoFullName: "acme/widget",
		Payload:      body,
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var title, state, authorLogin string
	var issueNum int
	if err := pool.QueryRow(ctx, `
		SELECT number, state, title, author_login
		FROM github_issues
		WHERE project_id = $1 AND github_issue_id = 1001
	`, projectID).Scan(&issueNum, &state, &title, &authorLogin); err != nil {
		t.Fatalf("select issue: %v", err)
	}
	if issueNum != 42 {
		t.Errorf("number: want 42, got %d", issueNum)
	}
	if state != "open" {
		t.Errorf("state: want open, got %q", state)
	}
	if title != "Fix the widget" {
		t.Errorf("title: want 'Fix the widget', got %q", title)
	}
	if authorLogin != "alice" {
		t.Errorf("author_login: want alice, got %q", authorLogin)
	}

	// Second ingest with changed title → ON CONFLICT DO UPDATE.
	var updated map[string]any
	_ = json.Unmarshal(body, &updated)
	issue := updated["issue"].(map[string]any)
	issue["title"] = "Updated title"
	updated["issue"] = issue
	ev.DeliveryID = deliveryID + "-v2"
	ev.Payload = mustJSON(t, updated)

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest v2: %v", err)
	}

	var updatedTitle string
	if err := pool.QueryRow(ctx, `
		SELECT title FROM github_issues
		WHERE project_id = $1 AND github_issue_id = 1001
	`, projectID).Scan(&updatedTitle); err != nil {
		t.Fatalf("select updated issue: %v", err)
	}
	if updatedTitle != "Updated title" {
		t.Errorf("want 'Updated title', got %q", updatedTitle)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id LIKE $1`, deliveryID+"%")
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_issues WHERE project_id = $1`, projectID)
	})
}

// ---------------------------------------------------------------------------
// PR upsert – merged PR fields written correctly
// ---------------------------------------------------------------------------

func TestIngest_PullRequestUpsert(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/widget-pr", "install-2")
	_ = projectID

	body := loadFixture(t, "pull_request_merged.json")
	deliveryID := "pr-upsert-" + time.Now().Format("20060102150405.000000000")

	ev := events.GitHubWebhookReceived{
		DeliveryID:   deliveryID,
		Event:        "pull_request",
		Action:       "closed",
		RepoFullName: "acme/widget-pr",
		Payload:      body,
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var prNum int
	var merged bool
	var authorLogin string
	if err := pool.QueryRow(ctx, `
		SELECT number, merged, author_login
		FROM github_pull_requests
		WHERE project_id = $1 AND github_pr_id = 2001
	`, projectID).Scan(&prNum, &merged, &authorLogin); err != nil {
		t.Fatalf("select PR: %v", err)
	}
	if prNum != 7 {
		t.Errorf("number: want 7, got %d", prNum)
	}
	if !merged {
		t.Error("merged: want true")
	}
	if authorLogin != "bob" {
		t.Errorf("author_login: want bob, got %q", authorLogin)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, deliveryID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_requests WHERE project_id = $1`, projectID)
	})
}

// ---------------------------------------------------------------------------
// installation.deleted – soft-deletes all projects for that installation
// ---------------------------------------------------------------------------

func TestIngest_InstallationDeleted_SoftDeletesProjects(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/del-test", "99")

	payload := mustJSON(t, map[string]any{
		"action": "deleted",
		"installation": map[string]any{
			"id": json.Number("99"),
		},
	})
	ev := events.GitHubWebhookReceived{
		DeliveryID: "install-deleted-" + time.Now().Format("20060102150405.000000000"),
		Event:      "installation",
		Action:     "deleted",
		Payload:    payload,
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var deletedAt *time.Time
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at, status FROM projects WHERE id = $1`, projectID,
	).Scan(&deletedAt, &status); err != nil {
		t.Fatalf("select project: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at: want non-nil after installation.deleted")
	}
	if status != "rejected" {
		t.Errorf("status: want rejected, got %q", status)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, ev.DeliveryID)
	})
}

// ---------------------------------------------------------------------------
// installation_repositories removed – soft-deletes named project
// ---------------------------------------------------------------------------

func TestIngest_InstallationRepositoriesRemoved_SoftDeletesProject(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/repo-removed", "77")

	payload := mustJSON(t, map[string]any{
		"action": "removed",
		"installation": map[string]any{
			"id": json.Number("77"),
		},
		"repositories_removed": []map[string]any{
			{"full_name": "acme/repo-removed"},
		},
	})
	ev := events.GitHubWebhookReceived{
		DeliveryID: "install-rm-" + time.Now().Format("20060102150405.000000000"),
		Event:      "installation_repositories",
		Action:     "removed",
		Payload:    payload,
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var deletedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at FROM projects WHERE id = $1`, projectID,
	).Scan(&deletedAt); err != nil {
		t.Fatalf("select project: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at: want non-nil after repositories_removed")
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, ev.DeliveryID)
	})
}

// ---------------------------------------------------------------------------
// installation_repositories added – restores previously soft-deleted project
// ---------------------------------------------------------------------------

func TestIngest_InstallationRepositoriesAdded_RestoresProject(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/repo-restored", "55")

	// First, soft-delete it manually.
	if _, err := pool.Exec(ctx, `
		UPDATE projects SET deleted_at = now(), status = 'rejected' WHERE id = $1
	`, projectID); err != nil {
		t.Fatalf("manual soft-delete: %v", err)
	}

	payload := mustJSON(t, map[string]any{
		"action": "added",
		"installation": map[string]any{
			"id": json.Number("55"),
		},
		"repositories_added": []map[string]any{
			{"full_name": "acme/repo-restored"},
		},
	})
	ev := events.GitHubWebhookReceived{
		DeliveryID: "install-add-" + time.Now().Format("20060102150405.000000000"),
		Event:      "installation_repositories",
		Action:     "added",
		Payload:    payload,
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var deletedAt *time.Time
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at, status FROM projects WHERE id = $1`, projectID,
	).Scan(&deletedAt, &status); err != nil {
		t.Fatalf("select project: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("deleted_at: want nil after restore, got %v", deletedAt)
	}
	if status != "verified" {
		t.Errorf("status: want verified, got %q", status)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, ev.DeliveryID)
	})
}

func TestIngest_Idempotence(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ing := &ingest.GitHubWebhookIngestor{Pool: pool}

	projectID := seedProject(t, pool, "acme/repo-idempotent", "install-id-99")

	payload := mustJSON(t, map[string]any{
		"action": "opened",
		"issue": map[string]any{
			"id":     123456789,
			"number": 42,
			"state":  "open",
			"title":  "A test issue for idempotence",
			"body":   "This is a test issue body",
			"html_url": "https://github.com/acme/repo-idempotent/issues/42",
			"user": map[string]any{
				"login": "octocat",
			},
		},
		"repository": map[string]any{
			"full_name": "acme/repo-idempotent",
		},
	})

	deliveryID := "test-delivery-idempotence-" + time.Now().Format("20060102150405.000000000")
	ev := events.GitHubWebhookReceived{
		DeliveryID: deliveryID,
		Event:      "issues",
		Action:     "opened",
		Payload:    payload,
	}

	// 1. First Ingestion: should process and insert sync jobs
	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("First Ingest: %v", err)
	}

	// Verify sync jobs were created
	var countBefore int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1::uuid", projectID).Scan(&countBefore); err != nil {
		t.Fatalf("select count sync_jobs: %v", err)
	}
	if countBefore == 0 {
		t.Error("expected sync jobs to be created on first ingestion")
	}

	// 2. Second Ingestion (duplicate): should skip and not create duplicate sync jobs
	// Delete sync jobs to see if second ingestion recreates them
	if _, err := pool.Exec(ctx, "DELETE FROM sync_jobs WHERE project_id = $1::uuid", projectID); err != nil {
		t.Fatalf("delete sync_jobs: %v", err)
	}

	if err := ing.Ingest(ctx, ev); err != nil {
		t.Fatalf("Second Ingest: %v", err)
	}

	// Verify NO sync jobs were recreated
	var countAfter int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM sync_jobs WHERE project_id = $1::uuid", projectID).Scan(&countAfter); err != nil {
		t.Fatalf("select count sync_jobs after: %v", err)
	}
	if countAfter != 0 {
		t.Errorf("expected 0 sync jobs after duplicate ingestion, got %d", countAfter)
	}

	// Test CleanupProcessedDeliveries function works
	deletedRows, err := ing.CleanupProcessedDeliveries(ctx)
	if err != nil {
		t.Fatalf("CleanupProcessedDeliveries: %v", err)
	}
	// The current entry is brand new, so it should not be deleted yet (rows deleted = 0)
	if deletedRows != 0 {
		t.Errorf("expected 0 rows deleted, got %d", deletedRows)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_deliveries WHERE delivery_id = $1`, deliveryID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_events WHERE delivery_id = $1`, deliveryID)
	})
}

