package ingest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/events"
	"github.com/jagadeesh/grainlify/backend/internal/ingest"
)

func TestIngest_IssueCommentEnqueuesSyncJobs(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	fullName := fmt.Sprintf("octo/issue-comment-enqueue-test-%s", uuid.New().String()[:8])
	var projectID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status)
VALUES ($1, $2, 'verified')
RETURNING id
`, ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	ingestor := &ingest.GitHubWebhookIngestor{Pool: d.Pool}

	err := ingestor.Ingest(ctx, events.GitHubWebhookReceived{
		DeliveryID:   "delivery-issue-comment-1",
		Event:        "issue_comment",
		Action:       "created",
		RepoFullName: fullName,
		Payload:      []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	rows, err := d.Pool.Query(ctx, `SELECT job_type FROM sync_jobs WHERE project_id = $1 ORDER BY job_type`, projectID)
	if err != nil {
		t.Fatalf("query sync_jobs: %v", err)
	}
	defer rows.Close()

	var jobTypes []string
	for rows.Next() {
		var jt string
		if err := rows.Scan(&jt); err != nil {
			t.Fatalf("scan job_type: %v", err)
		}
		jobTypes = append(jobTypes, jt)
	}

	want := []string{"sync_issues", "sync_prs"}
	if len(jobTypes) != len(want) {
		t.Fatalf("job types = %v, want %v", jobTypes, want)
	}
	for i, jt := range jobTypes {
		if jt != want[i] {
			t.Errorf("job types = %v, want %v", jobTypes, want)
			break
		}
	}
}

func TestIngest_IssueCommentWithUnresolvableRepoDoesNotEnqueue(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	ingestor := &ingest.GitHubWebhookIngestor{Pool: d.Pool}
	unresolvableRepo := fmt.Sprintf("octo/does-not-exist-in-grainlify-%s", uuid.New().String()[:8])

	// No project exists for this repo, so projectID resolution fails inside
	// Ingest() and nothing should be enqueued (and, critically, Ingest()
	// should not error - a webhook for an unregistered repo is routine, not
	// exceptional).
	err := ingestor.Ingest(ctx, events.GitHubWebhookReceived{
		DeliveryID:   "delivery-issue-comment-unresolvable",
		Event:        "issue_comment",
		Action:       "created",
		RepoFullName: unresolvableRepo,
		Payload:      []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var count int
	if err := d.Pool.QueryRow(ctx, `
SELECT count(*) FROM sync_jobs sj
JOIN projects p ON p.id = sj.project_id
WHERE p.github_full_name = $1
`, unresolvableRepo).Scan(&count); err != nil {
		t.Fatalf("count sync_jobs: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// Each delivery used to insert its own (sync_issues, sync_prs) pair. A burst
// of deliveries for one project must leave one pending job per type, delayed
// so the burst coalesces.
func TestIngest_BurstOfWebhooksQueuesOnePendingJobPerType(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var ownerID, projectID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('contributor') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	fullName := fmt.Sprintf("octo/webhook-burst-%s", uuid.New().String()[:8])
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		ownerID, fullName).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	ingestor := &ingest.GitHubWebhookIngestor{Pool: d.Pool}
	for i, ev := range []string{"pull_request", "push", "issue_comment", "issues", "pull_request", "push"} {
		if err := ingestor.Ingest(ctx, events.GitHubWebhookReceived{
			DeliveryID:   fmt.Sprintf("burst-%s-%d", projectID, i),
			Event:        ev,
			Action:       "opened",
			RepoFullName: fullName,
			Payload:      []byte(`{}`),
		}); err != nil {
			t.Fatalf("Ingest %s: %v", ev, err)
		}
	}

	rows, err := d.Pool.Query(ctx, `
SELECT job_type, count(*), bool_and(run_at > now() + interval '20 seconds')
FROM sync_jobs WHERE project_id = $1 AND status = 'pending' GROUP BY job_type ORDER BY job_type`, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var jt string
		var n int
		var delayed bool
		if err := rows.Scan(&jt, &n, &delayed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[jt] = n
		if !delayed {
			t.Errorf("%s job is not delayed by the webhook coalescing window", jt)
		}
	}
	if got["sync_issues"] != 1 || got["sync_prs"] != 1 || len(got) != 2 {
		t.Errorf("pending jobs after six webhooks = %v, want one of each type", got)
	}
}
