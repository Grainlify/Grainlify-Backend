package hackathon

import (
	"context"
	"log/slog"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// reconcileInterval is how often Reconciler re-enqueues sync_jobs for
// hackathon-relevant projects. A package constant rather than a config key
// for this slice - trivially made configurable later if needed.
const reconcileInterval = 5 * time.Minute

// Reconciler periodically re-enqueues sync_issues jobs for every accepted
// project in an active hackathon phase, so issue-prep label changes get
// picked up even if no other GitHub webhook happens to fire in between
// (AI-specs.md §2.2: "plus a periodic reconciliation crawl to catch missed
// webhooks. Do not rely on webhooks alone."). It does no work itself beyond
// that - everything downstream reuses syncjobs.Worker's existing, proven
// FOR UPDATE SKIP LOCKED claim/execute loop unchanged.
type Reconciler struct {
	pool db.DBPool
}

func NewReconciler(pool db.DBPool) *Reconciler {
	return &Reconciler{pool: pool}
}

// Run blocks, ticking every reconcileInterval until ctx is cancelled. Meant
// to be started in its own goroutine, mirroring syncjobs.Worker.Run's shape.
func (r *Reconciler) Run(ctx context.Context) error {
	if r.pool == nil {
		return nil
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				slog.Warn("hackathon reconciler tick failed", "error", err)
			}
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) error {
	// NOT EXISTS guards against unbounded duplicate enqueueing between
	// ticks - a rare race between two reconciler instances both passing
	// this check is harmless (worst case, one extra cheap resync), matching
	// how sync_jobs' own FOR UPDATE SKIP LOCKED claim already tolerates
	// concurrent workers.
	_, err := r.pool.Exec(ctx, `
INSERT INTO sync_jobs (project_id, job_type, status, run_at)
SELECT DISTINCT hpa.project_id, 'sync_issues', 'pending', now()
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
WHERE hpa.status = 'accepted' AND h.phase IN ('issue_prep', 'live')
  AND NOT EXISTS (
    SELECT 1 FROM sync_jobs sj
    WHERE sj.project_id = hpa.project_id AND sj.job_type = 'sync_issues' AND sj.status IN ('pending', 'running')
  )
`)
	return err
}
