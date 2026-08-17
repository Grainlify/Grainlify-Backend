package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every surface that lists or counts projects must exclude forks.
//
// #445 added the exclusion to three ranking gates and stopped there. Every
// other surface kept showing forks: Browse, recommendations, both ecosystem
// counts and the admin counts - and recommended ISSUES inherited the project
// list client-side, so forks reached the first page a new contributor sees.
//
// The structural guard in internal/ranking covers the ranking queries. This
// one covers the handler surfaces, because the mistake was not "a condition
// was written wrongly" - it was "a whole file was never considered".
//
// A file listed here that stops reading projects should be removed from the
// list deliberately, not left to pass vacuously.
func TestEveryProjectSurfaceExcludesForks(t *testing.T) {
	// file -> the project-reading statements in it that must carry the filter.
	surfaces := map[string][]string{
		"projects_public.go": {
			"split_part(p.github_full_name, '/', 2) != '.github'", // Browse/List conditions
			"ORDER BY contributors_count DESC",                    // Recommended
		},
		"ecosystems_public.go": {
			"LEFT JOIN projects p ON p.ecosystem_id = e.id",              // Explore list count
			"(SELECT COUNT(*) FROM projects p WHERE p.ecosystem_id = $1", // detail count
		},
		"admin_ecosystems.go": {
			"SELECT COUNT(p.id), COUNT(DISTINCT p.owner_user_id) FROM projects p",
		},
	}

	fork := regexp.MustCompile(`COALESCE\((?:p\.)?is_fork, FALSE\) = FALSE|ranking\.NotAFork(?:Condition)?`)

	for file, anchors := range surfaces {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v — if this file moved, move the guard with it rather than dropping it", file, err)
		}
		text := string(src)

		for _, anchor := range anchors {
			idx := strings.Index(text, anchor)
			if idx < 0 {
				t.Errorf("%s: anchor %q not found — the query was restructured. "+
					"Re-point this guard deliberately; leaving it unmatched makes it check nothing.", file, anchor)
				continue
			}
			// Look in a window around the anchor rather than the whole file, so
			// a filter on one query cannot vouch for another in the same file.
			start := idx - 1200
			if start < 0 {
				start = 0
			}
			end := idx + 1200
			if end > len(text) {
				end = len(text)
			}
			if !fork.MatchString(text[start:end]) {
				t.Errorf("%s: the query at %q reads projects without excluding forks.\n\n"+
					"A fork is somebody else's project under a contributor's namespace. Listing or "+
					"counting them inflates every public number and puts other people's repositories "+
					"on a contributor's page.", file, anchor)
			}
		}
	}
}
