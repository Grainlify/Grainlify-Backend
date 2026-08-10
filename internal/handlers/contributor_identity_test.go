package handlers_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawAuthorLoginDistinct matches a DISTINCT over an author_login column that
// is not wrapped in LOWER(): "DISTINCT a.author_login", "DISTINCT
// i.author_login", "DISTINCT author_login".
var rawAuthorLoginDistinct = regexp.MustCompile(`(?i)DISTINCT\s+(?:[a-z_]+\.)?author_login`)

// Guards against the casing-split bug reappearing.
//
// A shared helper does not prevent this on its own - the next person writes
// the SQL inline, as happened in nine places across five files before anyone
// noticed. This scans the source instead, so a tenth site fails the build
// rather than quietly splitting one contributor into two on a page nobody
// checks.
//
// If a genuine case-sensitive DISTINCT is ever needed, the fix is to say so
// in a comment on the line and widen this test deliberately - not to delete
// it.
func TestNoRawAuthorLoginDistinct(t *testing.T) {
	roots := []string{".", "../hackathon", "../syncjobs", "../ingest"}
	var offenders []string

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(src), "\n") {
				if !rawAuthorLoginDistinct.MatchString(line) {
					continue
				}
				// LOWER() on the same line is the correct form.
				if strings.Contains(strings.ToUpper(line), "LOWER(") {
					continue
				}
				offenders = append(offenders, filepath.Clean(path)+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("DISTINCT over a raw author_login splits one contributor across capitalisations.\n"+
			"Wrap it in LOWER() (see handlers.ContributorLoginKey). Offending lines:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
