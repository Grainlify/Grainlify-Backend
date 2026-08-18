package migrate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Migration versions are chosen by hand, and hands have now failed twice.
//
// golang-migrate refuses to run when two files claim the same version, so a
// collision is not a merge conflict that resolves itself - it is the API
// failing to boot, after the merge, in production, on a deploy that looked
// exactly like every other one. Git will not flag it either: two branches each
// adding a different file is a clean merge.
//
// This is the cheap half of the guard: it catches a collision that has already
// landed in one tree. The expensive half - catching it BEFORE the merge, while
// the number is still free to change - is a CI step that compares this
// branch's versions against every other remote branch, because a test can only
// ever see the tree it runs in.

var migrationFile = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

func migrationsDir(t *testing.T) string {
	t.Helper()
	// internal/migrate -> repo root
	dir := filepath.Join("..", "..", "migrations")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("migrations dir not found from here: %v", err)
	}
	return dir
}

// One version, one migration. Two files claiming the same number is the state
// that stops the service from starting.
func TestMigrations_NoDuplicateVersions(t *testing.T) {
	dir := migrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	names := map[uint64]map[string]bool{}
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Errorf("%s: unparsable version %q", e.Name(), m[1])
			continue
		}
		if names[v] == nil {
			names[v] = map[string]bool{}
		}
		names[v][m[2]] = true
	}

	var bad []uint64
	for v, set := range names {
		if len(set) > 1 {
			bad = append(bad, v)
		}
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i] < bad[j] })
	for _, v := range bad {
		var which []string
		for n := range names[v] {
			which = append(which, n)
		}
		sort.Strings(which)
		t.Errorf("version %d is claimed by %d different migrations: %s\n"+
			"golang-migrate refuses to run with a duplicate version, so this is the API failing to boot "+
			"rather than a merge conflict. Renumber the newer one to the next free version.",
			v, len(which), strings.Join(which, ", "))
	}
}

// firstVersionRequiringADown is where the convention starts.
//
// Versions 1-11 predate it and have no .down.sql. They are not being
// backfilled: writing a speculative rollback for an eight-month-old schema
// change nobody will ever run is worse than admitting the boundary, and a test
// that fails on history teaches people to skip the suite rather than to write
// downs. Everything from 12 onward has both, and must keep both.
//
// If this number ever needs raising, that is the signal to stop and ask why -
// it should only ever fall.
const firstVersionRequiringADown = 12

// Every up has a down and vice versa, from the point the convention began. A
// missing down is discovered when somebody needs to roll back, which is the
// worst possible moment - and it is also exactly what a half-finished renumber
// leaves behind, which is the case this was written for.
func TestMigrations_EveryVersionHasBothDirections(t *testing.T) {
	dir := migrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	up, down := map[uint64]string{}, map[uint64]string{}
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.ParseUint(m[1], 10, 64)
		if m[3] == "up" {
			up[v] = e.Name()
		} else {
			down[v] = e.Name()
		}
	}
	for v, name := range up {
		if v < firstVersionRequiringADown {
			continue
		}
		if _, ok := down[v]; !ok {
			t.Errorf("%s has no matching .down.sql", name)
		}
	}
	for v, name := range down {
		if _, ok := up[v]; !ok {
			t.Errorf("%s has no matching .up.sql - a leftover from a rename?", name)
		}
	}
}
