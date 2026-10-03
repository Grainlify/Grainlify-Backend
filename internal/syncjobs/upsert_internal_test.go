package syncjobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// fakeRepo serves a repository's issues, pull requests and comments the way
// GitHub's list endpoints do, and counts the calls made against it.
type fakeRepo struct {
	mu       sync.Mutex
	repo     string
	issues   []string // JSON objects, in listing order
	prs      []string
	comments map[int]string // issue number -> JSON array
	calls    map[string]int
}

func newFakeRepo(repo string, n int) *fakeRepo {
	f := &fakeRepo{repo: repo, comments: map[int]string{}, calls: map[string]int{}}
	for i := 1; i <= n; i++ {
		f.issues = append(f.issues, fakeIssue(repo, i, fmt.Sprintf("Issue %d", i), 2, "2026-09-01T00:00:00Z"))
		f.comments[i] = fmt.Sprintf(`[{"id":%d,"body":"first","user":{"login":"bob"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"},{"id":%d,"body":"second","user":{"login":"carol"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}]`, 10000+i, 20000+i)
		f.prs = append(f.prs, fakePR(repo, 1000+i, fmt.Sprintf("PR %d", i), "2026-09-01T00:00:00Z"))
	}
	return f
}

func fakeIssue(repo string, number int, title string, comments int, updatedAt string) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"state":"open","title":%q,"body":"body","html_url":"https://github.com/%s/issues/%d",
"user":{"login":"alice"},"assignees":[],"labels":[{"name":"bug","color":"d73a4a"}],"comments":%d,
"created_at":"2026-08-01T00:00:00Z","updated_at":%q,"closed_at":null}`, 500000+number, number, title, repo, number, comments, updatedAt)
}

func fakePR(repo string, number int, title, updatedAt string) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"state":"open","title":%q,"body":"body","html_url":"https://github.com/%s/pull/%d",
"user":{"login":"alice"},"merged_at":null,"merge_commit_sha":null,"head":{"sha":"abc"},
"created_at":"2026-08-01T00:00:00Z","updated_at":%q,"closed_at":null}`, 700000+number, number, title, repo, number, updatedAt)
}

func (f *fakeRepo) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	body, code := `{"message":"Not Found"}`, http.StatusNotFound
	page := r.URL.Query().Get("page")
	switch {
	case path == "/repos/"+f.repo+"/issues":
		f.calls["list issues"]++
		body, code = "[]", http.StatusOK
		if page == "1" {
			body = "[" + strings.Join(f.issues, ",") + "]"
		}
	case path == "/repos/"+f.repo+"/pulls":
		f.calls["list prs"]++
		body, code = "[]", http.StatusOK
		if page == "1" {
			body = "[" + strings.Join(f.prs, ",") + "]"
		}
	case strings.HasPrefix(path, "/repos/"+f.repo+"/issues/") && strings.HasSuffix(path, "/comments"):
		f.calls["list comments"]++
		var n int
		fmt.Sscanf(strings.TrimPrefix(path, "/repos/"+f.repo+"/issues/"), "%d", &n)
		body, code = f.comments[n], http.StatusOK
	case path == "/repos/"+f.repo+"/languages":
		body, code = `{"Go":100}`, http.StatusOK
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (f *fakeRepo) takeCalls() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = map[string]int{}
	return c
}

// rowVersions maps each row id to its xmin. An UPDATE that rewrites a row
// gives it a new xmin; a conflict whose DO UPDATE ... WHERE is false does not.
func rowVersions(t *testing.T, d *db.DB, table string, project uuid.UUID) map[string]string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(), `SELECT id::text, xmin::text FROM `+table+` WHERE project_id = $1`, project)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, xmin string
		if err := rows.Scan(&id, &xmin); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = xmin
	}
	return out
}

func rewritten(before, after map[string]string) int {
	n := 0
	for id, x := range after {
		if b, ok := before[id]; !ok || b != x {
			n++
		}
	}
	return n
}

func walBytes(t *testing.T, d *db.DB, fn func()) int64 {
	t.Helper()
	var start string
	if err := d.Pool.QueryRow(context.Background(), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&start); err != nil {
		t.Fatalf("lsn: %v", err)
	}
	fn()
	var n int64
	if err := d.Pool.QueryRow(context.Background(), `SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint`, start).Scan(&n); err != nil {
		t.Fatalf("lsn diff: %v", err)
	}
	return n
}

type syncFixture struct {
	d       *db.DB
	project uuid.UUID
	repo    string
	gh      *fakeRepo
	w       *Worker
}

func newSyncFixture(t *testing.T, n int) syncFixture {
	t.Helper()
	f := newQueueFixture(t)
	var repo string
	if err := f.d.Pool.QueryRow(context.Background(), `SELECT github_full_name FROM projects WHERE id = $1`, f.project).Scan(&repo); err != nil {
		t.Fatalf("repo: %v", err)
	}
	gh := newFakeRepo(repo, n)
	w := &Worker{
		pool:     f.d.Pool,
		limiter:  rate.NewLimiter(rate.Inf, 1),
		gh:       &github.Client{HTTP: &http.Client{Transport: gh}, UserAgent: "test"},
		workerID: "test",
	}
	return syncFixture{d: f.d, project: f.project, repo: repo, gh: gh, w: w}
}

func (s syncFixture) syncAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := s.w.syncIssues(ctx, s.project, s.repo, "tok", ""); err != nil {
		t.Fatalf("syncIssues: %v", err)
	}
	if err := s.w.syncPRs(ctx, s.project, s.repo, "tok"); err != nil {
		t.Fatalf("syncPRs: %v", err)
	}
}

// Every sync rewrote every issue and pull request of the repository, changed
// or not: 6.2M row versions on 26k github_issues rows, 4.0M on 17.5k pull
// requests. A sync of an unchanged repository now rewrites nothing, and a
// sync after one change rewrites that one row.
func TestSync_UnchangedRepositoryRewritesNoRows(t *testing.T) {
	const n = 60
	s := newSyncFixture(t, n)

	firstWAL := walBytes(t, s.d, func() { s.syncAll(t) })
	issues0 := rowVersions(t, s.d, "github_issues", s.project)
	prs0 := rowVersions(t, s.d, "github_pull_requests", s.project)
	if len(issues0) != n || len(prs0) != n {
		t.Fatalf("first sync stored %d issues and %d PRs, want %d each", len(issues0), len(prs0), n)
	}

	againWAL := walBytes(t, s.d, func() { s.syncAll(t) })
	issues1 := rowVersions(t, s.d, "github_issues", s.project)
	prs1 := rowVersions(t, s.d, "github_pull_requests", s.project)
	if got := rewritten(issues0, issues1); got != 0 {
		t.Errorf("re-syncing an unchanged repository rewrote %d of %d issue rows, want 0", got, n)
	}
	if got := rewritten(prs0, prs1); got != 0 {
		t.Errorf("re-syncing an unchanged repository rewrote %d of %d PR rows, want 0", got, n)
	}

	s.gh.mu.Lock()
	s.gh.issues[7] = fakeIssue(s.repo, 8, "Issue 8, retitled", 2, "2026-09-02T00:00:00Z")
	s.gh.prs[3] = fakePR(s.repo, 1004, "PR 4, retitled", "2026-09-02T00:00:00Z")
	s.gh.mu.Unlock()
	s.syncAll(t)
	if got := rewritten(issues1, rowVersions(t, s.d, "github_issues", s.project)); got != 1 {
		t.Errorf("one retitled issue rewrote %d rows, want 1", got)
	}
	if got := rewritten(prs1, rowVersions(t, s.d, "github_pull_requests", s.project)); got != 1 {
		t.Errorf("one retitled PR rewrote %d rows, want 1", got)
	}
	var title string
	if err := s.d.Pool.QueryRow(context.Background(), `SELECT title FROM github_issues WHERE project_id = $1 AND number = 8`, s.project).Scan(&title); err != nil || title != "Issue 8, retitled" {
		t.Errorf("retitled issue stored as %q (%v)", title, err)
	}

	t.Logf("%d issues + %d PRs: first sync wrote %d bytes of WAL, an unchanged re-sync %d bytes", n, n, firstWAL, againWAL)
}
