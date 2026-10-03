package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The crawl queues a sync_issues job for each accepted project of a live
// event, skips projects that already have one pending or running, and never
// adds a second pending row - which the unique index would refuse, failing the
// whole statement for every project.
func TestReconcilerTick_QueuesOncePerProject(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var owner, project, hackathonID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		owner, "octo/reconcile-tick-"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name, phase, starts_at, ends_at) VALUES ($1, 'live', $2, $3) RETURNING id`,
		"reconcile-tick-"+uuid.NewString(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour)).Scan(&hackathonID); err != nil {
		t.Fatalf("insert hackathon: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, 'desc', 'goal', 1, 'contact@example.com', 'accepted')`, hackathonID, project, owner); err != nil {
		t.Fatalf("insert application: %v", err)
	}

	pending := func() int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM sync_jobs WHERE project_id = $1 AND job_type = 'sync_issues' AND status = 'pending'`, project).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	r := NewReconciler(d.Pool)
	for i := 0; i < 3; i++ {
		if err := r.tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if got := pending(); got != 1 {
		t.Fatalf("pending after three ticks = %d, want 1", got)
	}

	// Running, not pending: the crawl leaves it alone.
	if _, err := d.Pool.Exec(ctx, `UPDATE sync_jobs SET status = 'running', locked_at = now() WHERE project_id = $1`, project); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick while running: %v", err)
	}
	if got := pending(); got != 0 {
		t.Errorf("pending while a sync_issues job runs = %d, want 0", got)
	}
}
