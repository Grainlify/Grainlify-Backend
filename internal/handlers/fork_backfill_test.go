package handlers

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// seedForkTestOwner creates the user a project must belong to.
func seedForkTestOwner(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor','fork-backfill-test',$1) RETURNING id
`, int64(uuid.New().ID())).Scan(&id); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

// The backfill exists so the exclusion is not a column nothing reads.
func TestForkBackfill_ResolvesUnknownProjects(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	installation := "inst-" + uuid.NewString()[:8]
	owner := seedForkTestOwner(t, d)

	newProject := func(name string) uuid.UUID {
		var id uuid.UUID
		if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, github_app_installation_id)
VALUES ($1, $2, 'verified', $3) RETURNING id`, owner, name, installation).Scan(&id); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id) })
		return id
	}

	suffix := uuid.NewString()[:8]
	forkID := newProject("someone/forked-" + suffix)
	realID := newProject("someone/original-" + suffix)

	b := &ForkBackfiller{
		db:    d,
		token: func(context.Context, string) (string, error) { return "t", nil },
		fetch: func(_ context.Context, _, fullName string) (bool, error) {
			return strings.Contains(fullName, "forked-"), nil
		},
	}
	b.Run(ctx)

	read := func(id uuid.UUID) *bool {
		var v *bool
		if err := d.Pool.QueryRow(ctx, `SELECT is_fork FROM projects WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("read is_fork: %v", err)
		}
		return v
	}
	if v := read(forkID); v == nil || !*v {
		t.Errorf("fork was not marked as one (is_fork = %v); it still counts toward ranking", v)
	}
	if v := read(realID); v == nil || *v {
		t.Errorf("a non-fork was marked as a fork (is_fork = %v); real work would stop counting", v)
	}
}

// The failure path, which is the one that decides whether this is safe.
//
// An installation we cannot read, or a repo GitHub will not answer for, must
// leave the row UNKNOWN. Writing false there would be the system quietly
// asserting something nobody checked - and it would re-open the vector for
// exactly the repositories we know least about.
func TestForkBackfill_UnreadableProjectsStayUnknownRatherThanAssumedSafe(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		b    func(*ForkBackfiller)
	}{
		{"installation token fails", func(b *ForkBackfiller) {
			b.token = func(context.Context, string) (string, error) { return "", fmt.Errorf("404 not found") }
			b.fetch = func(context.Context, string, string) (bool, error) { return false, nil }
		}},
		{"repo fetch fails", func(b *ForkBackfiller) {
			b.token = func(context.Context, string) (string, error) { return "t", nil }
			b.fetch = func(context.Context, string, string) (bool, error) { return false, fmt.Errorf("403") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id uuid.UUID
			if err := d.Pool.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, github_full_name, status, github_app_installation_id)
VALUES ($1, $2, 'verified', $3) RETURNING id`,
				seedForkTestOwner(t, d), "someone/unreadable-"+uuid.NewString()[:8],
				"inst-"+uuid.NewString()[:8]).Scan(&id); err != nil {
				t.Fatalf("seed: %v", err)
			}
			t.Cleanup(func() { _, _ = d.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, id) })

			b := &ForkBackfiller{db: d}
			tc.b(b)
			b.Run(ctx)

			var v *bool
			if err := d.Pool.QueryRow(ctx, `SELECT is_fork FROM projects WHERE id = $1`, id).Scan(&v); err != nil {
				t.Fatalf("read: %v", err)
			}
			if v != nil {
				t.Errorf("is_fork = %v for a project we could not read; unknown must stay unknown, "+
					"not be recorded as a fact nobody established", *v)
			}
		})
	}
}

// Every path that marks a project verified must also record its fork state.
//
// Verified is exactly the gate ranking reads, so a write path that sets it
// without recording is_fork puts a project into the eligible set with an
// unknown - and therefore permitted - fork state. There is no behavioural test
// for the installation sync (it would need GitHub's API mocked end to end), so
// this asserts the shape instead.
func TestEveryVerifyingWriteRecordsForkState(t *testing.T) {
	files := []string{"github_app.go", "projects.go"}
	stmt := regexp.MustCompile(`(?s)UPDATE projects.{0,700}?WHERE id = \$1`)

	found := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range stmt.FindAllString(string(src), -1) {
			if !strings.Contains(m, "status = 'verified'") {
				continue
			}
			found++
			if !strings.Contains(m, "is_fork") {
				t.Errorf("%s: an UPDATE marks a project verified without recording is_fork.\n\n"+
					"Verified is the gate ranking reads. A project entering it with an unknown fork "+
					"state is eligible by default, which is the farming vector this change closed.\n\n%s", f, m)
			}
		}
	}
	if found < 4 {
		t.Errorf("found only %d verifying writes; expected at least 4 (two installation-sync paths, "+
			"two manual-verify paths). If they were restructured, update this guard deliberately "+
			"rather than letting it check fewer things.", found)
	}
}
