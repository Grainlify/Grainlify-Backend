package migrate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jagadeesh/grainlify/backend/migrations"
)

const syncJobsOnePendingVersion = 20261003122120

// Production had 38,145 pending sync_jobs over 206 (project, job type) pairs
// when the unique index arrived. A CREATE UNIQUE INDEX over that fails, a
// failed migration leaves schema_migrations dirty, and a dirty database
// blocks every deploy after it. So the migration has to be run against a
// database that looks like production did: stopped one version short,
// seeded with duplicates, then migrated forward.
func TestSyncJobsOnePendingMigration_CollapsesExistingDuplicates(t *testing.T) {
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping test that needs a live database")
	}
	ctx := context.Background()

	// A database of its own: the shared test database is already migrated
	// to head, and this test needs one that is not.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)
	name := "mig_sync_jobs_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs: %v", err)
	}
	prev, err := src.Prev(syncJobsOnePendingVersion)
	if err != nil {
		t.Fatalf("no migration before %d: %v", syncJobsOnePendingVersion, err)
	}
	drv, err := postgres.WithInstance(sqlDB, &postgres.Config{MigrationsTable: "schema_migrations"})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(prev); err != nil {
		t.Fatalf("migrate to %d: %v", prev, err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var owner, projA, projB string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO users (role) VALUES ('maintainer') RETURNING id`).Scan(&owner); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, p := range []*string{&projA, &projB} {
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
			owner, "octo/mig-"+uuid.NewString()[:8]).Scan(p); err != nil {
			t.Fatalf("insert project: %v", err)
		}
	}
	// Project A: the firehose shape - five pending sync_issues, three pending
	// sync_prs, one running sync_issues, one completed of each.
	for i := 0; i < 5; i++ {
		mustExec(`INSERT INTO sync_jobs (project_id, job_type, status, run_at) VALUES ($1, 'sync_issues', 'pending', now() - make_interval(mins => $2))`, projA, 10-i)
	}
	for i := 0; i < 3; i++ {
		mustExec(`INSERT INTO sync_jobs (project_id, job_type, status, run_at) VALUES ($1, 'sync_prs', 'pending', now() - make_interval(mins => $2))`, projA, 10-i)
	}
	mustExec(`INSERT INTO sync_jobs (project_id, job_type, status, locked_at, locked_by) VALUES ($1, 'sync_issues', 'running', now() - interval '3 days', 'gone:1')`, projA)
	mustExec(`INSERT INTO sync_jobs (project_id, job_type, status) VALUES ($1, 'sync_issues', 'completed'), ($1, 'sync_prs', 'completed')`, projA)
	// Project B: already clean - one pending per type. Must be untouched.
	mustExec(`INSERT INTO sync_jobs (project_id, job_type, status) VALUES ($1, 'sync_issues', 'pending'), ($1, 'sync_prs', 'pending')`, projB)

	var oldestIssues string
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM sync_jobs WHERE project_id = $1 AND job_type = 'sync_issues' AND status = 'pending' ORDER BY run_at LIMIT 1`, projA).Scan(&oldestIssues); err != nil {
		t.Fatalf("oldest: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("migrate up over duplicates: %v", err)
	}
	if v, dirty, _ := m.Version(); dirty || v < syncJobsOnePendingVersion {
		t.Fatalf("after up: version %d dirty %v", v, dirty)
	}

	counts := func(project string) map[string]int {
		t.Helper()
		rows, err := sqlDB.QueryContext(ctx, `SELECT job_type || '/' || status, count(*) FROM sync_jobs WHERE project_id = $1 GROUP BY 1`, project)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[k] = n
		}
		return out
	}
	wantA := map[string]int{
		"sync_issues/pending": 1, "sync_issues/cancelled": 4, "sync_issues/running": 1, "sync_issues/completed": 1,
		"sync_prs/pending": 1, "sync_prs/cancelled": 2, "sync_prs/completed": 1,
	}
	if got := counts(projA); fmt.Sprint(got) != fmt.Sprint(wantA) {
		t.Errorf("project A after migration = %v, want %v", got, wantA)
	}
	wantB := map[string]int{"sync_issues/pending": 1, "sync_prs/pending": 1}
	if got := counts(projB); fmt.Sprint(got) != fmt.Sprint(wantB) {
		t.Errorf("project B after migration = %v, want %v", got, wantB)
	}

	var kept string
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM sync_jobs WHERE project_id = $1 AND job_type = 'sync_issues' AND status = 'pending'`, projA).Scan(&kept); err != nil {
		t.Fatalf("kept: %v", err)
	}
	if kept != oldestIssues {
		t.Errorf("kept pending job %s, want the oldest %s", kept, oldestIssues)
	}
	var missingReason int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM sync_jobs WHERE status = 'cancelled' AND (last_error IS NULL OR last_error NOT LIKE 'coalesced:%')`).Scan(&missingReason); err != nil {
		t.Fatalf("reason: %v", err)
	}
	if missingReason != 0 {
		t.Errorf("%d cancelled rows carry no coalesced: reason", missingReason)
	}

	// The constraint now holds: a second pending row for a pair is refused,
	// and ON CONFLICT against the partial index makes it a no-op.
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO sync_jobs (project_id, job_type) VALUES ($1, 'sync_prs')`, projB); err == nil {
		t.Error("a second pending sync_prs for project B was accepted; the unique index is missing")
	}
	res, err := sqlDB.ExecContext(ctx, `INSERT INTO sync_jobs (project_id, job_type) VALUES ($1, 'sync_prs') ON CONFLICT (project_id, job_type) WHERE status = 'pending' DO NOTHING`, projB)
	if err != nil {
		t.Fatalf("ON CONFLICT insert: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("ON CONFLICT insert affected %d rows, want 0", n)
	}
}
