package useremail_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// users.email may be read in two places and nowhere else.
//
// The address is collected for one purpose: sending somebody the notifications
// they asked for. The risk is not that someone deliberately publishes it - it
// is that a maintainer-facing applicant list, an admin export or a public
// profile picks up `SELECT ... email ... FROM users` because the column was
// there and looked useful. Nothing about that reads as a mistake in review,
// and nothing fails when it ships.
//
// So the guard is structural, in the same shape as the one over
// payout_contact_email: the column is read through this package, and a second
// reader anywhere in the repository fails this test. Someone who needs one has
// to add it to the list below, which is a decision somebody makes rather than
// a line somebody slips in.
func TestStoredEmailHasOneReaderPerPurpose(t *testing.T) {
	root := repoRoot(t)

	// A SQL statement that touches the users table and names a bare `email`
	// column. Bare: payout_contact_email and notification_preferences.email
	// are different columns with different rules, and are not this.
	usersTable := regexp.MustCompile(`(?is)\b(from|update|into)\s+users\b`)
	bareEmail := regexp.MustCompile(`(?i)(^|[^_a-z])email\b`)
	rawString := regexp.MustCompile("(?s)`[^`]*`")

	allowed := map[string]string{
		"internal/useremail/useremail.go":   "the package that owns the column",
		"internal/notifications/service.go": "sendEmail, the one thing the address was collected for",
		"internal/handlers/github_oauth.go": "capture at sign-in (through useremail.Capture, no SQL of its own)",
		"internal/handlers/auth.go":         "capture at resync (through useremail.Capture, no SQL of its own)",
	}

	var offenders []string
	var scanned int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "migrations":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for _, stmt := range rawString.FindAllString(string(b), -1) {
			if !usersTable.MatchString(stmt) || !bareEmail.MatchString(stmt) {
				continue
			}
			if _, ok := allowed[rel]; ok {
				continue
			}
			offenders = append(offenders, rel+": "+firstLine(stmt))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A walk that found nothing finds no offenders either, which is
	// indistinguishable from a clean repository.
	if scanned < 100 {
		t.Fatalf("scanned %d Go files; the walk is not running where it thinks it is", scanned)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("users.email is read in %d place(s) outside the allowed list.\n"+
			"The address was collected to send somebody their own notifications. If this "+
			"reader serves that purpose, add it to `allowed` with a reason; if it puts the "+
			"address in front of anybody other than its owner, it must not ship:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "`"))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root (no go.mod above the test)")
	return ""
}
