package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The sync must persist the fields it decodes.
//
// InstallationRepository decodes Description and Topics, and the INSERT simply
// never listed description - so every project created before that fix has
// none, and each was invisible to the catalogue until a human typed one in.
// Third field found decoded and dropped, after fork and owner.type.
//
// Nothing compared the INSERT's column list against the struct, which is why
// all three survived review. This asserts the columns that must be there; the
// fuller version - every decoded field either persisted or explicitly listed
// as intentionally dropped - is in the handoff.
func TestSyncInsertPersistsTheFieldsItDecodes(t *testing.T) {
	src, err := os.ReadFile("github_app.go")
	if err != nil {
		t.Fatalf("read github_app.go: %v", err)
	}
	text := string(src)

	m := regexp.MustCompile(`(?s)INSERT INTO projects \((.*?)\)`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("no INSERT INTO projects found in github_app.go - if the sync was restructured, " +
			"re-point this guard rather than deleting it")
	}
	columns := m[1]

	// Decoded by InstallationRepository and meaningful to a reader of the
	// catalogue. is_fork is covered separately by the ranking guards.
	// Each was decoded from the GitHub payload before it was persisted, and
	// four were dropped silently for months: fork, owner.type, description and
	// repository_selection. This is where that pattern stops.
	for _, col := range []string{
		"github_full_name", "language", "tags", "description",
		"installation_repository_selection",
	} {
		if !strings.Contains(columns, col) {
			t.Errorf("the sync INSERT does not persist %q, although InstallationRepository decodes it.\n\n"+
				"A field parsed and dropped is invisible: fork cost a farmable leaderboard, owner.type "+
				"blocked owner grouping, and description left 106 projects out of the catalogue.\n\n"+
				"Columns were:\n%s", col, strings.TrimSpace(columns))
		}
	}
}
