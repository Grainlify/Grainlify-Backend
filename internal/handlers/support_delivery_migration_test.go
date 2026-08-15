package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The third copy of the rule.
//
// supportUndeliveredPredicate and supportFullyDelivered are checked against
// each other in Postgres by TestSupportDelivered_SQLAndGoAgree - but that test
// evaluates the GO CONSTANT as SQL. It never reads the migration, so the index
// that actually exists in the database could say something entirely different
// and nothing would notice. The comment claiming the two are byte-identical
// was, until this test, enforced by nothing.
//
// That gap is the same shape as the bug this whole file exists to prevent: a
// rule written down twice, with only a comment promising the copies agree.
func TestUndeliveredIndexMatchesTheGoPredicate(t *testing.T) {
	const path = "../../migrations/000064_support_delivery_semantics.up.sql"

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// The WHERE clause of the CREATE INDEX, minus the trailing semicolon.
	re := regexp.MustCompile(`(?s)CREATE INDEX[^;]*?\bWHERE\s+(.*?);`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no CREATE INDEX ... WHERE found in %s - if the index was renamed or dropped, this test needs to follow it rather than be deleted", path)
	}

	inMigration := normalizeSQL(string(m[1]))
	inGo := normalizeSQL(supportUndeliveredPredicate)

	if inMigration != inGo {
		t.Errorf(`the index predicate and supportUndeliveredPredicate have drifted.

migration: %s
go:        %s

These must stay identical: the Go constant is what any replay query uses, and
the migration is what the database actually indexes. If the rule changed,
change both.`, inMigration, inGo)
	}
}

// normalizeSQL collapses whitespace so indentation differences between a Go
// raw string and a .sql file don't read as drift. Nothing else is normalized -
// a changed column, operator or literal must still fail.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
