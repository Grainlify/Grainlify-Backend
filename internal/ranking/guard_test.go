package ranking_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rankingPkg is the import path this package must not appear under
// internal/hackathon.
const rankingPkg = "github.com/jagadeesh/grainlify/backend/internal/ranking"

// TestHackathonDoesNotImportRanking enforces a standing rule: leaderboard
// rank must never feed draw weights, maintainer-pool scoring, application
// outcomes, payouts, or eligibility.
//
// The rule is not about tidiness. Rank is a public, cheaply-gamed vanity
// metric computed from data Grainlify does not control; the draw is designed
// so that assignment odds come from fit, prior completion and abandon history
// instead. Wiring rank into any of that would hand a farmer a lever - grind
// merges in a repo you control, or in whatever counts this month, and buy
// better odds or money with it. It would also silently break the promise the
// public docs now make ("Rank does not affect assignment odds or payouts").
//
// It is clean today. This test is what keeps it clean by construction rather
// than by whoever reviews the next pull request happening to remember. A
// failure here is not a lint error to route around: if hackathon code needs a
// contribution count, it should compute the one it actually means, with its
// own anti-gaming properties, rather than borrow the leaderboard's.
func TestHackathonDoesNotImportRanking(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "hackathon"))
	if err != nil {
		t.Fatalf("resolve internal/hackathon: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("internal/hackathon not found at %s: %v - if the package moved, "+
			"move this guard with it rather than deleting it", root, err)
	}

	var offenders []string

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		// Parse imports only - build.ImportDir would need the whole package to
		// typecheck, and this guard must keep working while the tree is mid-edit.
		pkg, perr := parseImports(path)
		if perr != nil {
			return perr
		}
		for _, imp := range pkg {
			if imp == rankingPkg {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/hackathon: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("internal/hackathon must not import %s, but these files do:\n  %s\n\n"+
			"Leaderboard rank is presentational and must never become an input to "+
			"draw weights, maintainer-pool scoring, application outcomes, payouts, "+
			"or eligibility. If you need a contribution signal inside the hackathon "+
			"package, define one there with its own anti-gaming properties.",
			rankingPkg, strings.Join(offenders, "\n  "))
	}
}

// parseImports returns the import paths of a single Go file.
//
// parser.ImportsOnly, so the guard keeps working on a tree that does not
// currently typecheck - a broken build must not be able to quietly disable
// the rule this test enforces.
func parseImports(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}
