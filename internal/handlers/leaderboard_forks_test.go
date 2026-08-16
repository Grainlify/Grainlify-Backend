package handlers_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// A fork is a repository the contributor controls.
//
// They can open a pull request against it and merge it themselves, with nobody
// reviewing anything. Ranking counts merged pull requests precisely BECAUSE a
// merge means somebody else accepted the work - so a merge inside a fork
// carries none of the meaning the number is supposed to have, and counting it
// turns the one metric the platform claims is unfarmable into one anyone can
// mint on demand: install the App on all repositories, fork any repo, merge
// into it.
//
// This is not a theoretical shape. The App installation grants whatever the
// user did not deselect, "All repositories" is GitHub's default, and the
// installation sync creates AND auto-verifies a project for every repo it can
// see. One production installation carries 30 projects of which 26 are forks
// of other organisations' repositories.
//
// If this test fails, the leaderboard is farmable. It is not a style rule and
// it must not be relaxed to make an unrelated change pass.

// setProjectFork flips the stored fork flag on a seeded project.
func setProjectFork(t *testing.T, pool db.DBPool, projectID uuid.UUID, isFork bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE projects SET is_fork = $2, fork_checked_at = now() WHERE id = $1`, projectID, isFork); err != nil {
		t.Fatalf("setProjectFork: %v", err)
	}
}

func TestLeaderboardRules_MergedPullRequestsInAForkDoNotCount(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)

	fork := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")
	setProjectFork(t, d.Pool, fork, true)

	// Seeded well above the single-merge tail, so that if the exclusion
	// regressed this login would rank high enough for a bounded search to find
	// it - absence is then proof rather than an artefact of giving up early.
	const farmed = 8
	farmer := "lbsuite-forkfarmer-" + uuid.New().String()[:8]
	for i := 0; i < farmed; i++ {
		leaderboardSuitePR(t, d.Pool, fork, farmer)
	}

	if e := leaderboardSuiteFindRanked(t, app, farmer, farmed); e != nil {
		t.Errorf("%q is ranked at position %v with %v merged PRs, all of them merged into a fork they control; "+
			"the leaderboard is farmable by anyone who installs the App on all repositories and merges into a fork",
			farmer, e["rank"], e["merged_prs"])
	}
}

// The other half of the property. An exclusion that also drops real work is
// not a fix, and "no fork ranks" is trivially satisfiable by ranking nobody.
func TestLeaderboardRules_MergesInARealRepositoryStillCount(t *testing.T) {
	d := testDB(t)
	app := newLeaderboardSuiteApp(d)

	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	owner := leaderboardSuiteUser(t, d.Pool)

	const merges = 8

	// Everything is seeded BEFORE the first query. The endpoint caches a
	// computed ranking for 60s per scope, so seeding between two searches gets
	// the second one a stale page - which reads exactly like an exclusion bug.
	real := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")
	setProjectFork(t, d.Pool, real, false)
	contributor := "lbsuite-realwork-" + uuid.New().String()[:8]

	// Deliberately left NULL: fork state never determined.
	unknown := leaderboardSuiteProject(t, d.Pool, owner, ecoID, "verified")
	unresolved := "lbsuite-unresolved-" + uuid.New().String()[:8]

	for i := 0; i < merges; i++ {
		leaderboardSuitePR(t, d.Pool, real, contributor)
		leaderboardSuitePR(t, d.Pool, unknown, unresolved)
	}

	if e := leaderboardSuiteFindRanked(t, app, contributor, merges); e == nil {
		t.Errorf("%q was excluded despite %d merges in a repository that is not a fork", contributor, merges)
	}

	// Unknown fork state still counts. That is the documented, deliberate
	// default: making unknown ineligible would empty the board between the
	// column being added and the backfill completing. If this ever becomes the
	// wrong trade, change it knowingly - do not discover it because the
	// leaderboard went blank on deploy.
	if e := leaderboardSuiteFindRanked(t, app, unresolved, merges); e == nil {
		t.Errorf("%q was excluded although the project's fork state is unknown (NULL); "+
			"unknown is deliberately eligible - see notAFork in internal/ranking", unresolved)
	}
}
