package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// seedInstallTestUser creates a user with a linked GitHub account, which is
// where the install callback learns who the installer is on GitHub.
func seedInstallTestUser(t *testing.T, d *db.DB, login string) uuid.UUID {
	t.Helper()
	ghID := int64(uuid.New().ID())
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor', $1, $2) RETURNING id
`, login, ghID).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1, $2, $3, '\x00')
`, id, ghID, login); err != nil {
		t.Fatalf("seed github account: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM projects WHERE owner_user_id = $1`, id)
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// fakeInstallation is one GitHub App installation as the sync sees it: the
// repositories it covers, and who may do what on each.
func fakeInstallation(d *db.DB, repos []github.InstallationRepository, perms map[string]map[string]string) *GitHubAppHandler {
	return &GitHubAppHandler{
		db:    d,
		token: func(context.Context, string) (string, error) { return "installation-token", nil },
		listRepos: func(context.Context, string) ([]github.InstallationRepository, string, error) {
			return repos, "all", nil
		},
		repoPermission: func(_ context.Context, _, fullName, login string) (string, error) {
			if p, ok := perms[fullName][login]; ok {
				return p, nil
			}
			return "none", nil
		},
	}
}

func publicRepo(fullName string) github.InstallationRepository {
	return github.InstallationRepository{ID: int64(uuid.New().ID()), FullName: fullName}
}

// readProject returns a project's owner and status, or ok=false when there is
// no such project.
func readProject(t *testing.T, d *db.DB, fullName string) (owner uuid.UUID, status string, ok bool) {
	t.Helper()
	err := d.Pool.QueryRow(context.Background(),
		`SELECT owner_user_id, status FROM projects WHERE github_full_name = $1`, fullName).Scan(&owner, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false
	}
	if err != nil {
		t.Fatalf("read project %s: %v", fullName, err)
	}
	return owner, status, true
}

// The install callback maps the OAuth state to a user, but nothing tied the
// installation_id in the same URL to that user. Anyone could start an install,
// swap in another account's installation_id, and have that account's
// repositories registered as verified projects owned by them.
func TestInstallationSync_IgnoresRepositoriesTheInstallerCannotMaintain(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	victimLogin, intruderLogin := "victim-"+suffix, "intruder-"+suffix
	victim := seedInstallTestUser(t, d, victimLogin)
	intruder := seedInstallTestUser(t, d, intruderLogin)

	// One repository not registered yet, and one the victim registered and
	// later removed, which a sync would otherwise restore and re-verify.
	repo, removed := "victim-"+suffix+"/widgets", "victim-"+suffix+"/retired"
	if _, err := d.Pool.Exec(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, deleted_at) VALUES ($1, $2, 'rejected', now())
`, victim, removed); err != nil {
		t.Fatalf("seed removed project: %v", err)
	}
	h := fakeInstallation(d, []github.InstallationRepository{publicRepo(repo), publicRepo(removed)},
		map[string]map[string]string{repo: {victimLogin: "admin"}, removed: {victimLogin: "admin"}})

	h.syncInstallationRepositories(ctx, intruder, "victims-installation")

	if owner, status, ok := readProject(t, d, repo); ok {
		t.Errorf("%s was registered (owner %s, status %s) for a user with no permission on it; "+
			"the installation belongs to someone else", repo, owner, status)
	}
	var status string
	var deleted bool
	if err := d.Pool.QueryRow(ctx, `SELECT status, deleted_at IS NOT NULL FROM projects WHERE github_full_name = $1`,
		removed).Scan(&status, &deleted); err != nil {
		t.Fatalf("read removed project: %v", err)
	}
	if status != "rejected" || !deleted {
		t.Errorf("%s: status=%q deleted=%v, want it left rejected and deleted by someone else's installation",
			removed, status, deleted)
	}
}

// The same check must not get in the way of the people the flow is for.
func TestInstallationSync_RegistersRepositoriesTheInstallerMaintains(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	login := "maintainer-" + suffix
	user := seedInstallTestUser(t, d, login)

	adminRepo, writeRepo, readRepo := login+"/admin", "org-"+suffix+"/write", "org-"+suffix+"/read"
	h := fakeInstallation(d,
		[]github.InstallationRepository{publicRepo(adminRepo), publicRepo(writeRepo), publicRepo(readRepo)},
		map[string]map[string]string{
			adminRepo: {login: "admin"},
			writeRepo: {login: "write"},
			readRepo:  {login: "read"},
		})

	h.syncInstallationRepositories(ctx, user, "own-installation")

	for _, repo := range []string{adminRepo, writeRepo} {
		owner, status, ok := readProject(t, d, repo)
		if !ok || owner != user || status != "verified" {
			t.Errorf("%s: ok=%v owner=%s status=%q, want a verified project owned by the installer", repo, ok, owner, status)
		}
	}
	if _, _, ok := readProject(t, d, readRepo); ok {
		t.Errorf("%s was registered for a user who can only read it", readRepo)
	}
}

// An unreadable permission is unknown, and unknown is not "yes".
func TestInstallationSync_UnreadablePermissionRegistersNothing(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	user := seedInstallTestUser(t, d, "maintainer-"+suffix)

	repo := "org-" + suffix + "/widgets"
	h := fakeInstallation(d, []github.InstallationRepository{publicRepo(repo)}, nil)
	h.repoPermission = func(context.Context, string, string, string) (string, error) {
		return "", errors.New("403 Resource not accessible by integration")
	}

	h.syncInstallationRepositories(ctx, user, "some-installation")

	if _, _, ok := readProject(t, d, repo); ok {
		t.Errorf("%s was registered although the installer's permission could not be read", repo)
	}
}

// POST /projects needs no proof, so anyone can register a repository first
// and sit on it unverified. When a real maintainer then installs the App, the
// sync verified the squatter's row and left them its owner - a verified
// project, with the maintainer's installation, belonging to someone who
// proved nothing. An unverified row goes to the installer GitHub vouched for;
// a verified one stays with the maintainer who verified it.
func TestInstallationSync_ProvenInstallerTakesOverOnlyUnverifiedProjects(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	login := "maintainer-" + suffix
	installer := seedInstallTestUser(t, d, login)
	squatter := seedInstallTestUser(t, d, "squatter-"+suffix)
	coMaintainer := seedInstallTestUser(t, d, "co-maintainer-"+suffix)

	squatted, shared := "org-"+suffix+"/squatted", "org-"+suffix+"/shared"
	for _, p := range []struct {
		owner        uuid.UUID
		repo, status string
	}{{squatter, squatted, "pending_verification"}, {coMaintainer, shared, "verified"}} {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, $3)`,
			p.owner, p.repo, p.status); err != nil {
			t.Fatalf("seed %s: %v", p.repo, err)
		}
	}
	h := fakeInstallation(d, []github.InstallationRepository{publicRepo(squatted), publicRepo(shared)},
		map[string]map[string]string{squatted: {login: "admin"}, shared: {login: "admin"}})

	h.syncInstallationRepositories(ctx, installer, "own-installation")

	if owner, status, _ := readProject(t, d, squatted); owner != installer || status != "verified" {
		t.Errorf("%s: owner=%s status=%q, want verified and owned by the installer (%s), not the squatter (%s)",
			squatted, owner, status, installer, squatter)
	}
	if owner, status, _ := readProject(t, d, shared); owner != coMaintainer || status != "verified" {
		t.Errorf("%s: owner=%s status=%q, want it left with the maintainer who verified it (%s)",
			shared, owner, status, coMaintainer)
	}
}
