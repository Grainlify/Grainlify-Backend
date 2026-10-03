package syncjobs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

type queueFixture struct {
	d       *db.DB
	project uuid.UUID
}

func newQueueFixture(t *testing.T) queueFixture {
	t.Helper()
	d := dbtest.FreshDB(t)
	return queueFixture{d: d, project: newQueueProject(t, d)}
}

func newQueueProject(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var owner, project uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		owner, "octo/queue-"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return project
}

func (f queueFixture) insert(t *testing.T, project uuid.UUID, jobType, status string, lockedAgo time.Duration, lockedBy string, attempts int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	var lockedAt any
	if status == "running" {
		lockedAt = time.Now().Add(-lockedAgo)
	}
	if err := f.d.Pool.QueryRow(context.Background(), `
INSERT INTO sync_jobs (project_id, job_type, status, run_at, locked_at, locked_by, attempts)
VALUES ($1, $2, $3, now(), $4, NULLIF($5, ''), $6) RETURNING id`,
		project, jobType, status, lockedAt, lockedBy, attempts).Scan(&id); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return id
}

type jobRow struct {
	Status    string
	Attempts  int
	LastError string
	LockedBy  string
}

func (f queueFixture) job(t *testing.T, id uuid.UUID) jobRow {
	t.Helper()
	var r jobRow
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status, attempts, COALESCE(last_error, ''), COALESCE(locked_by, '') FROM sync_jobs WHERE id = $1`, id).
		Scan(&r.Status, &r.Attempts, &r.LastError, &r.LockedBy); err != nil {
		t.Fatalf("read job: %v", err)
	}
	return r
}

func testWorker(pool db.DBPool, tm queueTiming, exec func(context.Context, claimedJob) error) *Worker {
	return &Worker{pool: pool, workerID: "test-worker:" + uuid.NewString()[:6], timing: tm, exec: exec}
}

// Production had 111 jobs 'running' under containers that no longer
// existed, the oldest for nine months. Every one of them must come back on
// its own.
func TestReapExpired_TakesBackJobsWhoseWorkerIsGone(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	other := newQueueProject(t, f.d)
	third := newQueueProject(t, f.d)

	stale := f.insert(t, f.project, "sync_issues", "running", 9*24*time.Hour, "gone-container:1", 0)
	fresh := f.insert(t, f.project, "sync_prs", "running", 30*time.Second, "alive:1", 0)
	// A pair whose stale job already has a pending successor: the successor
	// covers it, so the stale one is cancelled rather than duplicated.
	coveredStale := f.insert(t, other, "sync_issues", "running", time.Hour, "gone:2", 0)
	successor := f.insert(t, other, "sync_issues", "pending", 0, "", 0)
	// Two stale jobs for one pair (19 such pairs in production): one comes
	// back, the other is cancelled.
	twinA := f.insert(t, third, "sync_prs", "running", 2*time.Hour, "gone:3", 0)
	twinB := f.insert(t, third, "sync_prs", "running", time.Hour, "gone:4", 0)
	// A job that keeps dying is not retried forever.
	exhausted := f.insert(t, third, "sync_issues", "running", time.Hour, "gone:5", maxAttempts-1)

	w := testWorker(f.d.Pool, queueTiming{leaseTimeout: 5 * time.Minute}, nil)
	n, err := w.reapExpired(ctx)
	if err != nil {
		t.Fatalf("reapExpired: %v", err)
	}
	if n != 5 {
		t.Errorf("reaped %d jobs, want 5", n)
	}

	if r := f.job(t, stale); r.Status != "pending" || r.Attempts != 1 || r.LockedBy != "" || !strings.Contains(r.LastError, "lease expired: worker gone-container:1") {
		t.Errorf("stale job = %+v, want pending, attempts 1, lease-expired reason", r)
	}
	if r := f.job(t, fresh); r.Status != "running" || r.LockedBy != "alive:1" {
		t.Errorf("job with a live lease = %+v, want untouched", r)
	}
	if r := f.job(t, coveredStale); r.Status != "cancelled" || !strings.Contains(r.LastError, "superseded") {
		t.Errorf("stale job with a pending successor = %+v, want cancelled as superseded", r)
	}
	if r := f.job(t, successor); r.Status != "pending" {
		t.Errorf("successor = %+v, want still pending", r)
	}
	a, b := f.job(t, twinA), f.job(t, twinB)
	if !(a.Status == "pending" && b.Status == "cancelled") && !(a.Status == "cancelled" && b.Status == "pending") {
		t.Errorf("twin stale jobs = %s / %s, want one pending and one cancelled", a.Status, b.Status)
	}
	if r := f.job(t, exhausted); r.Status != "failed" || !strings.Contains(r.LastError, "giving up") {
		t.Errorf("job out of attempts = %+v, want failed, giving up", r)
	}

	// Idempotent: nothing left to reap.
	if n, err := w.reapExpired(ctx); err != nil || n != 0 {
		t.Errorf("second reap = %d, %v; want 0, nil", n, err)
	}
}

// A healthy job may run far longer than the lease (sync_issues has taken
// eleven minutes in production). The heartbeat is what keeps it.
func TestHeartbeat_KeepsALongJobFromBeingReaped(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	id := f.insert(t, f.project, "sync_issues", "pending", 0, "", 0)

	tm := queueTiming{heartbeat: 20 * time.Millisecond, leaseTimeout: 150 * time.Millisecond}
	stopReaper := make(chan struct{})
	reaperDone := make(chan struct{})
	w := testWorker(f.d.Pool, tm, func(ctx context.Context, j claimedJob) error {
		select {
		case <-time.After(800 * time.Millisecond): // > 5 leases
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	reaper := testWorker(f.d.Pool, tm, nil)
	go func() {
		defer close(reaperDone)
		for {
			select {
			case <-stopReaper:
				return
			case <-time.After(30 * time.Millisecond):
				if _, err := reaper.reapExpired(ctx); err != nil {
					t.Errorf("reap: %v", err)
				}
			}
		}
	}()
	ran, err := w.processOne(ctx)
	close(stopReaper)
	<-reaperDone
	if err != nil || !ran {
		t.Fatalf("processOne = %v, %v", ran, err)
	}
	if r := f.job(t, id); r.Status != "completed" || r.Attempts != 1 {
		t.Errorf("long job = %+v, want completed by its own worker", r)
	}
}

// If the lease is taken back anyway (the worker stalled past it), the job
// is stopped and its eventual outcome does not overwrite the reaper's.
func TestHeartbeat_LostLeaseCancelsTheJob(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	id := f.insert(t, f.project, "sync_prs", "pending", 0, "", 0)

	started := make(chan struct{})
	var sawCancel atomic.Bool
	w := testWorker(f.d.Pool, queueTiming{heartbeat: 20 * time.Millisecond}, func(ctx context.Context, j claimedJob) error {
		close(started)
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})
	done := make(chan error, 1)
	go func() { _, err := w.processOne(ctx); done <- err }()
	<-started
	// What the reaper does to a stale job.
	if _, err := f.d.Pool.Exec(ctx, `UPDATE sync_jobs SET status = 'pending', locked_by = NULL, locked_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("take back: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("processOne: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job kept running after its lease was taken back")
	}
	if !sawCancel.Load() {
		t.Error("job context was not cancelled")
	}
	if r := f.job(t, id); r.Status != "pending" {
		t.Errorf("job = %+v, want left pending as the reaper set it", r)
	}
}

// Shutdown hands the in-flight job back instead of abandoning it 'running'.
func TestRun_ShutdownReleasesTheJobInFlight(t *testing.T) {
	f := newQueueFixture(t)
	id := f.insert(t, f.project, "sync_issues", "pending", 0, "", 0)

	started := make(chan struct{})
	w := testWorker(f.d.Pool, queueTiming{minIdle: 10 * time.Millisecond}, func(ctx context.Context, j claimedJob) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never claimed the job")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	if r := f.job(t, id); r.Status != "pending" || r.Attempts != 0 || r.LockedBy != "" {
		t.Errorf("job after shutdown = %+v, want pending, no attempt spent, unlocked", r)
	}
}

// Two syncs of one repository at once only race each other's writes: a
// pending job waits while its pair is running, and others go ahead.
func TestClaim_SkipsAPairThatIsAlreadyRunning(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	other := newQueueProject(t, f.d)

	f.insert(t, f.project, "sync_issues", "running", time.Second, "busy:1", 0)
	blocked := f.insert(t, f.project, "sync_issues", "pending", 0, "", 0)
	if _, err := f.d.Pool.Exec(ctx, `UPDATE sync_jobs SET run_at = now() - interval '1 hour' WHERE id = $1`, blocked); err != nil {
		t.Fatalf("age blocked job: %v", err)
	}
	free := f.insert(t, other, "sync_issues", "pending", 0, "", 0)

	w := testWorker(f.d.Pool, queueTiming{}, nil)
	j, ok, err := w.claim(ctx)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if j.ID != free {
		t.Errorf("claimed %s, want the other project's job %s (the older one's pair is running)", j.ID, free)
	}
	if _, ok, _ := w.claim(ctx); ok {
		t.Error("claimed the blocked job while its pair is still running")
	}
}
