package syncjobs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func seedBacklog(t *testing.T, f queueFixture) (a, b, c string) {
	t.Helper()
	ctx := context.Background()
	other, third := newQueueProject(t, f.d), newQueueProject(t, f.d)
	// The firehose shape, without the unique index in the way: insert the
	// duplicates the old code made.
	if _, err := f.d.Pool.Exec(ctx, `DROP INDEX IF EXISTS uq_sync_jobs_one_pending`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.d.Pool.Exec(context.Background(), `CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_jobs_one_pending ON sync_jobs (project_id, job_type) WHERE status = 'pending'`)
	})
	for i := 0; i < 30; i++ {
		f.insert(t, f.project, "sync_issues", "pending", 0, "", 0)
		f.insert(t, f.project, "sync_prs", "pending", 0, "", 0)
	}
	// Stuck under gone workers: one covered by pending jobs, two twins with
	// nothing pending, and one still inside the threshold (a live job).
	f.insert(t, f.project, "sync_issues", "running", 9*24*time.Hour, "gone:1", 0)
	f.insert(t, other, "sync_prs", "running", 3*time.Hour, "gone:2", 0)
	f.insert(t, other, "sync_prs", "running", 2*time.Hour, "gone:3", 0)
	f.insert(t, third, "sync_issues", "running", 5*time.Minute, "alive:1", 0)
	f.insert(t, third, "sync_issues", "completed", 0, "", 1)
	return f.project.String(), other.String(), third.String()
}

func TestCleanup_DryRunReportsAndChangesNothing(t *testing.T) {
	f := newQueueFixture(t)
	seedBacklog(t, f)
	ctx := context.Background()

	r, err := Cleanup(ctx, f.d.Pool, time.Hour, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want := map[string]int64{ActionCancelDuplicatePending: 58, ActionRequeueStuckRunning: 1, ActionCancelStuckRunning: 2}
	for k, v := range want {
		if r.Planned[k] != v {
			t.Errorf("planned %s = %d, want %d", k, r.Planned[k], v)
		}
	}
	if r.Applied || len(r.After) != 0 {
		t.Errorf("dry run reported as applied: %+v", r)
	}
	again, err := Cleanup(ctx, f.d.Pool, time.Hour, false)
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if again.Before["pending/sync_issues"] != 30 || again.Planned[ActionCancelDuplicatePending] != 58 {
		t.Errorf("a dry run changed the table: %+v", again)
	}
}

func TestCleanup_ApplyCollapsesTheBacklogOnceAndIsIdempotent(t *testing.T) {
	f := newQueueFixture(t)
	_, other, third := seedBacklog(t, f)
	ctx := context.Background()

	r, err := Cleanup(ctx, f.d.Pool, time.Hour, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if r.Before["pending/sync_issues"] != 30 || r.After["pending/sync_issues"] != 1 || r.After["pending/sync_prs"] != 2 {
		t.Errorf("before %v / after %v", r.Before, r.After)
	}
	if r.After["cancelled/sync_issues"] != 30 || r.After["cancelled/sync_prs"] != 30 {
		t.Errorf("after %v, want 29 duplicates + 1 stuck cancelled per type", r.After)
	}

	// The twin stuck jobs of a pair with nothing pending: the newer one
	// comes back, the older one is cancelled.
	var st, reason string
	if err := f.d.Pool.QueryRow(ctx, `SELECT status, last_error FROM sync_jobs WHERE project_id = $1 AND locked_by IS NULL AND last_error LIKE '%gone:3%'`, other).Scan(&st, &reason); err != nil {
		t.Fatalf("twin: %v", err)
	}
	if st != "pending" || !strings.HasPrefix(reason, "reset: running since ") {
		t.Errorf("newer stuck twin = %s (%s), want pending with a reset reason", st, reason)
	}
	var live string
	if err := f.d.Pool.QueryRow(ctx, `SELECT status FROM sync_jobs WHERE project_id = $1 AND locked_by = 'alive:1'`, third).Scan(&live); err != nil || live != "running" {
		t.Errorf("job inside the threshold = %q (%v), want untouched", live, err)
	}

	// Satisfies the index the migration creates.
	if _, err := f.d.Pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_jobs_one_pending ON sync_jobs (project_id, job_type) WHERE status = 'pending'`); err != nil {
		t.Errorf("after cleanup the unique index cannot be built: %v", err)
	}

	again, err := Cleanup(ctx, f.d.Pool, time.Hour, true)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for k, n := range again.Planned {
		if n != 0 {
			t.Errorf("second apply planned %s = %d, want nothing", k, n)
		}
	}
}

// Run before the deploy, the table still has the old status CHECK.
func TestCleanup_ApplyWorksBeforeTheMigration(t *testing.T) {
	f := newQueueFixture(t)
	seedBacklog(t, f)
	ctx := context.Background()
	if _, err := f.d.Pool.Exec(ctx, `
ALTER TABLE sync_jobs DROP CONSTRAINT sync_jobs_status_check;
ALTER TABLE sync_jobs ADD CONSTRAINT sync_jobs_status_check CHECK (status IN ('pending', 'running', 'completed', 'failed'))`); err != nil {
		t.Fatalf("restore old check: %v", err)
	}
	r, err := Cleanup(ctx, f.d.Pool, time.Hour, true)
	if err != nil {
		t.Fatalf("apply on the old schema: %v", err)
	}
	if r.After["cancelled/sync_issues"] == 0 {
		t.Errorf("nothing cancelled: %+v", r.After)
	}
}
