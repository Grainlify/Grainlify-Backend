package syncqueue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/syncqueue"
)

func newProject(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var owner, project uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		owner, "octo/syncqueue-"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return project
}

func countJobs(t *testing.T, d *db.DB, project uuid.UUID, jobType, status string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sync_jobs WHERE project_id = $1 AND job_type = $2 AND status = $3`, project, jobType, status).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The firehose: every webhook used to add a pair. A burst of them must leave
// exactly one pending job per type.
func TestEnqueue_CoalescesIntoThePendingJob(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	project := newProject(t, d)

	n, err := syncqueue.Enqueue(ctx, d.Pool, project, 0, syncqueue.AllTypes...)
	if err != nil || n != 2 {
		t.Fatalf("first Enqueue = %d, %v; want 2 rows", n, err)
	}
	for i := 0; i < 50; i++ {
		n, err := syncqueue.Enqueue(ctx, d.Pool, project, 0, syncqueue.AllTypes...)
		if err != nil {
			t.Fatalf("Enqueue #%d: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("Enqueue #%d inserted %d rows while both types were pending", i, n)
		}
	}
	for _, jt := range syncqueue.AllTypes {
		if got := countJobs(t, d, project, jt, "pending"); got != 1 {
			t.Errorf("%s pending = %d, want 1", jt, got)
		}
	}

	// Types coalesce independently.
	if _, err := d.Pool.Exec(ctx, `UPDATE sync_jobs SET status = 'completed' WHERE project_id = $1 AND job_type = 'sync_prs'`, project); err != nil {
		t.Fatalf("complete prs: %v", err)
	}
	if n, err := syncqueue.Enqueue(ctx, d.Pool, project, 0, syncqueue.AllTypes...); err != nil || n != 1 {
		t.Fatalf("Enqueue after sync_prs completed = %d, %v; want 1 (only sync_prs)", n, err)
	}
}

// A running job may already have read past the change a webhook reports, so
// it must not absorb the request: one pending job queues behind it, and no
// more than one.
func TestEnqueue_QueuesOneBehindARunningJob(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	project := newProject(t, d)

	if _, err := syncqueue.Enqueue(ctx, d.Pool, project, 0, syncqueue.SyncIssues); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE sync_jobs SET status = 'running', locked_at = now() WHERE project_id = $1`, project); err != nil {
		t.Fatalf("start job: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := syncqueue.Enqueue(ctx, d.Pool, project, 0, syncqueue.SyncIssues); err != nil {
			t.Fatalf("Enqueue while running: %v", err)
		}
	}
	if got := countJobs(t, d, project, syncqueue.SyncIssues, "running"); got != 1 {
		t.Errorf("running = %d, want 1", got)
	}
	if got := countJobs(t, d, project, syncqueue.SyncIssues, "pending"); got != 1 {
		t.Errorf("pending behind the running job = %d, want 1", got)
	}
}

func TestEnqueue_DelayIsAppliedAndDoesNotSlideForward(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	project := newProject(t, d)

	if _, err := syncqueue.Enqueue(ctx, d.Pool, project, 30*time.Second, syncqueue.SyncPRs); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var first float64
	if err := d.Pool.QueryRow(ctx, `SELECT extract(epoch FROM run_at - now()) FROM sync_jobs WHERE project_id = $1`, project).Scan(&first); err != nil {
		t.Fatalf("run_at: %v", err)
	}
	if first < 25 || first > 31 {
		t.Fatalf("run_at is %.1fs from now, want ~30s", first)
	}
	// A later event must not push the pending job back: the delay counts from
	// the first event, or a busy repository would never sync.
	if _, err := syncqueue.Enqueue(ctx, d.Pool, project, 10*time.Minute, syncqueue.SyncPRs); err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	var second float64
	if err := d.Pool.QueryRow(ctx, `SELECT extract(epoch FROM run_at - now()) FROM sync_jobs WHERE project_id = $1`, project).Scan(&second); err != nil {
		t.Fatalf("run_at: %v", err)
	}
	if second > first+1 {
		t.Errorf("a later enqueue moved run_at from %.1fs to %.1fs", first, second)
	}
}

func TestWake_NeverBlocksAndCoalesces(t *testing.T) {
	for len(syncqueue.Wakeups()) > 0 {
		<-syncqueue.Wakeups()
	}
	for i := 0; i < 10; i++ {
		syncqueue.Wake()
	}
	select {
	case <-syncqueue.Wakeups():
	default:
		t.Fatal("no wakeup delivered")
	}
	select {
	case <-syncqueue.Wakeups():
		t.Fatal("ten Wakes delivered more than one wakeup")
	default:
	}
}
