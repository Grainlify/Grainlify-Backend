package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// Forks must not be recommended.
//
// A fork is a repository the contributor controls; #445 excluded them from
// ranking for that reason. Recommendations were never audited in that pass and
// had no coverage, so they kept surfacing forks on the Discover page - the
// first thing a new contributor sees - and recommended ISSUES inherit the
// list client-side, so the same repos leaked into that surface too.
//
// This is the one behavioural test for that predicate outside internal/ranking.
func TestRecommended_DoesNotRecommendForks(t *testing.T) {
	d := testDB(t)
	app := newProjectsPublicTestApp(config.Config{}, d)

	owner := adminSuiteInsertUser(t, d, "contributor")
	ecoID, _ := leaderboardSuiteEcosystem(t, d.Pool)
	suffix := uuid.NewString()[:8]
	stars := 500

	forkName := "forkowner-" + suffix + "/forked"
	realName := "realowner-" + suffix + "/original"

	ids := map[string]uuid.UUID{}
	for _, name := range []string{forkName, realName} {
		ids[name] = projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
			OwnerUserID: owner, GitHubFullName: name, EcosystemID: &ecoID,
			Status: "verified", NeedsMetadata: false, StarsCount: &stars,
		})
		// Recommendations order by contributors_count DESC. The shared test
		// database is seeded by other suites, so a project with zero
		// contributors sorts below all of them and falls outside any limit -
		// which failed this test in the full suite while passing it alone.
		// Both fixtures are seeded high enough to be found regardless.
		projectsFxSeedManyContributors(t, d.Pool, ids[name], 40)
	}
	// Only one of them is a fork.
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE projects SET is_fork = (github_full_name = $1), fork_checked_at = now()
		 WHERE github_full_name IN ($1, $2)`, forkName, realName); err != nil {
		t.Fatalf("set fork state: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(),
			`DELETE FROM projects WHERE github_full_name IN ($1,$2)`, forkName, realName)
	})

	status, body := projectsFxDoJSON(t, app, "GET", "/projects/recommended?limit=50", "", nil)
	if status != 200 {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var out struct {
		Projects []struct {
			GitHubFullName string `json:"github_full_name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	var sawFork, sawReal bool
	for _, p := range out.Projects {
		if p.GitHubFullName == forkName {
			sawFork = true
		}
		if p.GitHubFullName == realName {
			sawReal = true
		}
	}
	if sawFork {
		t.Error("a fork was recommended - Discover is the first surface a new contributor sees, " +
			"and recommended issues inherit this list client-side")
	}
	// The other half: an exclusion that also drops real projects is not a fix,
	// and "no fork is recommended" is trivially satisfiable by recommending
	// nothing.
	if !sawReal {
		t.Error("a non-fork project with 500 stars was not recommended; the exclusion is too broad " +
			"or the fixture is not reaching the query")
	}
}
