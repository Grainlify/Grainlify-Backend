package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	githubpkg "github.com/jagadeesh/grainlify/backend/internal/github"
)

// ---------------------------------------------------------------------------
// Fakes / stubs
// ---------------------------------------------------------------------------

// fakeTokenGetter implements installationTokenGetter and lets tests inject
// a canned response per installation ID.
type fakeTokenGetter struct {
	responses map[string]error // nil error == success
}

func (f *fakeTokenGetter) GetInstallationToken(_ context.Context, id string) (string, error) {
	if err, ok := f.responses[id]; ok {
		return "", err
	}
	return "tok-" + id, nil
}

// fakeRows implements pgx.Rows for tests, backed by a simple string slice.
type fakeRows struct {
	ids     []string
	pos     int
	scanErr error
	closed  bool
}

func newFakeRows(ids []string) *fakeRows { return &fakeRows{ids: ids} }

func (r *fakeRows) Next() bool {
	if r.pos < len(r.ids) {
		r.pos++
		return true
	}
	return false
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != 1 {
		return fmt.Errorf("fakeRows.Scan: expected 1 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ids[r.pos-1]
	return nil
}

func (r *fakeRows) Close()                              { r.closed = true }
func (r *fakeRows) Err() error                          { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag       { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)              { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                 { return nil }
func (r *fakeRows) Conn() *pgx.Conn                     { return nil }

// fakeCommandTag wraps pgconn.CommandTag with a preset RowsAffected value.
type fakeCommandTag struct{ rows int64 }

func (t fakeCommandTag) RowsAffected() int64      { return t.rows }
func (t fakeCommandTag) String() string            { return fmt.Sprintf("UPDATE %d", t.rows) }
func (t fakeCommandTag) Select() bool              { return false }
func (t fakeCommandTag) Insert() bool              { return false }
func (t fakeCommandTag) Update() bool              { return true }
func (t fakeCommandTag) Delete() bool              { return false }

// fakePool implements db.DBPool. Only Exec and Query are exercised here;
// other methods panic so tests fail loudly if they are called unexpectedly.
type fakePool struct {
	// query behaviour
	queryRows   pgx.Rows
	queryErr    error
	// exec behaviour
	execTag     pgconn.CommandTag
	execErr     error
	execCalled  bool
	execSQL     string
	execArgs    []any
}

func (p *fakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	return p.queryRows, p.queryErr
}

func (p *fakePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.execCalled = true
	p.execSQL = sql
	p.execArgs = args
	return p.execTag, p.execErr
}

func (p *fakePool) QueryRow(context.Context, string, ...any) pgx.Row { panic("not implemented") }
func (p *fakePool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("not implemented")
}
func (p *fakePool) Ping(context.Context) error { return nil }
func (p *fakePool) Close()                     {}
func (p *fakePool) Config() *pgxpool.Config    { return nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeHandler(pool *fakePool) *GitHubAppCleanupHandler {
	return &GitHubAppCleanupHandler{
		cfg:  config.Config{},
		pool: pool,
	}
}

// ---------------------------------------------------------------------------
// checkSingleInstallation tests
// ---------------------------------------------------------------------------

// TestCheckSingleInstallation_Gone verifies that a genuine 404 (ErrInstallationNotFound)
// causes the handler to UPDATE projects and mark them deleted.
func TestCheckSingleInstallation_Gone(t *testing.T) {
	pool := &fakePool{
		execTag: pgconn.NewCommandTag("UPDATE 2"),
	}
	h := makeHandler(pool)

	notFoundErr := &githubpkg.InstallationNotFoundError{
		InstallationID: "inst-42",
		StatusCode:     http.StatusNotFound,
	}
	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-42": notFoundErr},
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-42")

	if !pool.execCalled {
		t.Fatal("expected Exec to be called to mark projects deleted, but it was not")
	}
}

// TestCheckSingleInstallation_TransientError verifies that a non-404 error (e.g. network
// timeout, 5xx) does NOT trigger project deletion.
func TestCheckSingleInstallation_TransientError(t *testing.T) {
	pool := &fakePool{}
	h := makeHandler(pool)

	transient := fmt.Errorf("connection refused")
	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-7": transient},
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-7")

	if pool.execCalled {
		t.Fatal("Exec must NOT be called for a transient error — risk of mass-deleting active projects")
	}
}

// TestCheckSingleInstallation_ServerError verifies that an HTTP 500-style error
// (non-404) does NOT trigger project deletion.
func TestCheckSingleInstallation_ServerError(t *testing.T) {
	pool := &fakePool{}
	h := makeHandler(pool)

	serverErr := fmt.Errorf("failed to get installation token: status 500, error: internal server error")
	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-99": serverErr},
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-99")

	if pool.execCalled {
		t.Fatal("Exec must NOT be called for a 5xx error")
	}
}

// TestCheckSingleInstallation_AuthError verifies that a permission/auth error
// (HTTP 401/403) does NOT trigger project deletion.
func TestCheckSingleInstallation_AuthError(t *testing.T) {
	pool := &fakePool{}
	h := makeHandler(pool)

	authErr := fmt.Errorf("failed to get installation token: status 401, error: unauthorized")
	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-13": authErr},
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-13")

	if pool.execCalled {
		t.Fatal("Exec must NOT be called for an auth error")
	}
}

// TestCheckSingleInstallation_StillActive verifies that a successful token fetch
// (installation is healthy) never triggers project deletion.
func TestCheckSingleInstallation_StillActive(t *testing.T) {
	pool := &fakePool{}
	h := makeHandler(pool)

	getter := &fakeTokenGetter{
		responses: map[string]error{}, // no error → success
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-healthy")

	if pool.execCalled {
		t.Fatal("Exec must NOT be called for an active installation")
	}
}

// TestCheckSingleInstallation_DBExecError verifies that a DB error during the
// UPDATE does not panic and is handled gracefully.
func TestCheckSingleInstallation_DBExecError(t *testing.T) {
	pool := &fakePool{
		execErr: errors.New("db connection lost"),
	}
	h := makeHandler(pool)

	notFoundErr := &githubpkg.InstallationNotFoundError{
		InstallationID: "inst-bad-db",
		StatusCode:     http.StatusNotFound,
	}
	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-bad-db": notFoundErr},
	}

	// Should not panic.
	h.checkSingleInstallation(context.Background(), getter, "inst-bad-db")

	if !pool.execCalled {
		t.Fatal("Exec should still be attempted even when DB later returns an error")
	}
}

// TestCheckSingleInstallation_WrappedNotFoundError verifies that an error wrapping
// ErrInstallationNotFound (e.g. via fmt.Errorf %w) is still detected correctly.
func TestCheckSingleInstallation_WrappedNotFoundError(t *testing.T) {
	pool := &fakePool{
		execTag: pgconn.NewCommandTag("UPDATE 1"),
	}
	h := makeHandler(pool)

	inner := &githubpkg.InstallationNotFoundError{
		InstallationID: "inst-wrapped",
		StatusCode:     http.StatusNotFound,
	}
	wrapped := fmt.Errorf("outer context: %w", inner)

	getter := &fakeTokenGetter{
		responses: map[string]error{"inst-wrapped": wrapped},
	}

	h.checkSingleInstallation(context.Background(), getter, "inst-wrapped")

	if !pool.execCalled {
		t.Fatal("wrapped ErrInstallationNotFound must still trigger project deletion")
	}
}

// ---------------------------------------------------------------------------
// checkInstallations tests
// ---------------------------------------------------------------------------

// TestCheckInstallations_ZeroInstallations verifies that an empty DB result
// is a no-op (no GitHub API call, no Exec).
func TestCheckInstallations_ZeroInstallations(t *testing.T) {
	pool := &fakePool{
		queryRows: newFakeRows(nil),
	}
	h := makeHandler(pool)

	// checkInstallations needs a real *github.GitHubAppClient but we can't
	// inject the getter here because checkInstallations constructs it internally.
	// When the DB returns no rows the function returns early before any API call.
	h.checkInstallations(context.Background())

	if pool.execCalled {
		t.Fatal("Exec must not be called when there are no active installations")
	}
}

// TestCheckInstallations_QueryError verifies that a DB query failure is
// handled gracefully without panicking.
func TestCheckInstallations_QueryError(t *testing.T) {
	pool := &fakePool{
		queryErr: errors.New("db query failed"),
	}
	h := makeHandler(pool)

	// Should not panic.
	h.checkInstallations(context.Background())

	if pool.execCalled {
		t.Fatal("Exec must not be called when the query itself fails")
	}
}

// TestCheckInstallations_NilPool verifies that a nil pool doesn't panic.
func TestCheckInstallations_NilPool(t *testing.T) {
	h := &GitHubAppCleanupHandler{cfg: config.Config{}, pool: nil}

	// Should not panic.
	h.checkInstallations(context.Background())
}

// ---------------------------------------------------------------------------
// Sentinel / typed error API tests
// ---------------------------------------------------------------------------

// TestErrInstallationNotFound_IsDetectable confirms that errors.Is works for
// a direct *InstallationNotFoundError value.
func TestErrInstallationNotFound_IsDetectable(t *testing.T) {
	err := &githubpkg.InstallationNotFoundError{
		InstallationID: "42",
		StatusCode:     http.StatusNotFound,
	}

	if !errors.Is(err, githubpkg.ErrInstallationNotFound) {
		t.Fatal("errors.Is(err, ErrInstallationNotFound) must return true for *InstallationNotFoundError")
	}
}

// TestErrInstallationNotFound_WrappedIsDetectable confirms that errors.Is works
// through a fmt.Errorf %w wrap.
func TestErrInstallationNotFound_WrappedIsDetectable(t *testing.T) {
	inner := &githubpkg.InstallationNotFoundError{InstallationID: "7", StatusCode: 404}
	wrapped := fmt.Errorf("api layer: %w", inner)

	if !errors.Is(wrapped, githubpkg.ErrInstallationNotFound) {
		t.Fatal("errors.Is must find ErrInstallationNotFound through a %w wrapper")
	}
}

// TestOtherError_NotInstallationNotFound confirms that a generic error does NOT
// match the sentinel — i.e. no false positives.
func TestOtherError_NotInstallationNotFound(t *testing.T) {
	other := errors.New("some other error containing 404 and not found in the text")

	if errors.Is(other, githubpkg.ErrInstallationNotFound) {
		t.Fatal("a plain error must NOT match ErrInstallationNotFound")
	}
}
