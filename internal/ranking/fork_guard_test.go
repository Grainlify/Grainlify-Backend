package ranking_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every verified-project gate must also exclude forks.
//
// TestLeaderboardRules_MergedPullRequestsInAForkDoNotCount proves the property
// for the contributor board. This proves it for the file: a query added later,
// or an existing one edited, cannot quietly gate on `status = 'verified'`
// without the fork exclusion beside it.
//
// The two tests fail differently on purpose. The behavioural one fails when
// the leaderboard becomes farmable. This one fails when the SHAPE that keeps
// it unfarmable is broken - including in the org queries, which no behavioural
// test covers, and which are the ones most likely to be "simplified" by
// somebody who checked only that the contributor board still worked.
func TestEligibilityPredicateAppliesEverywhere(t *testing.T) {
	src, err := os.ReadFile("ranking.go")
	if err != nil {
		t.Fatalf("read ranking.go: %v - if this file moved, move this guard with it rather than deleting it", err)
	}
	text := string(src)

	// Both WHERE and AND introduce these gates. The first version of this
	// regex accepted only AND, so it matched one gate of three and passed while
	// the exclusion was missing from the other two - a check that ran, reported
	// success, and proved almost nothing.
	gate := regexp.MustCompile(`(?:WHERE|AND)\s+(p2?)\.status = 'verified'`)
	matches := gate.FindAllStringSubmatchIndex(text, -1)

	// The count is asserted, not just the contents. A restructure that folds a
	// gate into a view or a helper would otherwise reduce this to checking
	// whatever happened to be left.
	const wantGates = 3
	if len(matches) != wantGates {
		t.Fatalf("found %d verified-project gates in ranking.go, expected %d.\n"+
			"If a query was added, it needs the fork exclusion and this count needs updating. "+
			"If one was removed or restructured, update this guard deliberately - do not "+
			"let it silently check fewer things than it used to.", len(matches), wantGates)
	}

	for _, m := range matches {
		alias := text[m[2]:m[3]]
		line := strings.Count(text[:m[0]], "\n") + 1

		// The exclusion must appear within the same WHERE clause. Ten lines is
		// generous for these queries and still far short of reaching the next
		// gate.
		windowEnd := m[1]
		for i, n := m[1], 0; i < len(text) && n < 10; i++ {
			if text[i] == '\n' {
				n++
			}
			windowEnd = i
		}
		window := text[m[0]:windowEnd]

		want := "NotAFork"
		if alias == "p2" {
			want = "notAForkP2"
		}
		if !strings.Contains(window, want) {
			t.Errorf("ranking.go:%d gates on %s.status = 'verified' without %s beside it.\n\n"+
				"A verified project that is a FORK is a repository the contributor controls: they can "+
				"merge their own pull requests with nobody reviewing them. Any eligibility gate that "+
				"omits this makes the leaderboard farmable by installing the GitHub App on all "+
				"repositories and merging into a fork.\n\nClause was:\n%s",
				line, alias, want, window)
		}
	}

	// And the definitions themselves must still say what they are meant to say.
	// A guard that only checks a name is satisfied by a constant set to "".
	for _, def := range []string{
		`const NotAForkCondition = "COALESCE(p.is_fork, FALSE) = FALSE"`,
		`const notAForkP2 = "AND COALESCE(p2.is_fork, FALSE) = FALSE"`,
	} {
		if !strings.Contains(text, def) {
			t.Errorf("the exclusion constant no longer reads as expected; wanted to find:\n  %s\n"+
				"If the predicate legitimately changed, update this guard deliberately - "+
				"referencing a constant that no longer excludes anything is the failure mode "+
				"this check exists to catch.", def)
		}
	}
}
