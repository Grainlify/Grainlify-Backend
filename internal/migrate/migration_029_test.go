package migrate

import (
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/migrations"
)

// TestMigration029SQL verifies the SQL content of migration 000029 without
// requiring a live database connection.  It checks:
//   - The up migration creates exactly the expected indexes.
//   - The up migration drops the superseded idx_sync_jobs_pending.
//   - Every CREATE INDEX uses IF NOT EXISTS (idempotency).
//   - The down migration drops exactly the new indexes and nothing else.
//   - The down migration restores idx_sync_jobs_pending with IF NOT EXISTS.
func TestMigration029SQL(t *testing.T) {
	upSQL := readMigration(t, "000029_perf_claim_and_aggregation_indexes.up.sql")
	downSQL := readMigration(t, "000029_perf_claim_and_aggregation_indexes.down.sql")

	// ── up: expected new indexes ──────────────────────────────────────────────
	newIndexes := []string{
		"idx_sync_jobs_claim",
		"idx_github_issues_author_login_lower",
		"idx_github_prs_author_login_lower",
		"idx_github_accounts_login_lower",
	}
	for _, name := range newIndexes {
		if !containsCI(upSQL, "CREATE INDEX IF NOT EXISTS "+name) {
			t.Errorf("up migration: missing CREATE INDEX IF NOT EXISTS %s", name)
		}
	}

	// ── up: drops the superseded index from 000003 ───────────────────────────
	if !containsCI(upSQL, "DROP INDEX IF EXISTS idx_sync_jobs_pending") {
		t.Error("up migration: must drop idx_sync_jobs_pending")
	}

	// ── up: the old index must NOT be re-created ──────────────────────────────
	if containsCI(upSQL, "CREATE INDEX IF NOT EXISTS idx_sync_jobs_pending") {
		t.Error("up migration: must not re-create idx_sync_jobs_pending")
	}

	// ── up: idx_sync_jobs_claim must be partial (WHERE status = 'pending') ───
	claimBlock := extractIndexBlock(upSQL, "idx_sync_jobs_claim")
	if !containsCI(claimBlock, "WHERE status = 'pending'") {
		t.Error("up migration: idx_sync_jobs_claim must be a partial index WHERE status = 'pending'")
	}

	// ── up: LOWER() functional indexes must reference LOWER( ─────────────────
	for _, name := range []string{
		"idx_github_issues_author_login_lower",
		"idx_github_prs_author_login_lower",
		"idx_github_accounts_login_lower",
	} {
		block := extractIndexBlock(upSQL, name)
		if !containsCI(block, "LOWER(") {
			t.Errorf("up migration: %s must be a functional LOWER() index", name)
		}
	}

	// ── down: drops the four new indexes ─────────────────────────────────────
	for _, name := range newIndexes {
		if !containsCI(downSQL, "DROP INDEX IF EXISTS "+name) {
			t.Errorf("down migration: missing DROP INDEX IF EXISTS %s", name)
		}
	}

	// ── down: must NOT drop indexes from other migrations ────────────────────
	otherIndexes := []string{
		"idx_github_issues_author_login",
		"idx_github_prs_author_login",
		"idx_github_accounts_login",
		"idx_sync_jobs_project",
		"idx_github_issues_project",
		"idx_github_prs_project",
	}
	for _, name := range otherIndexes {
		// Allow the name only if it's a substring of one of the new indexes
		// (e.g. idx_github_issues_author_login is a prefix of the _lower variant).
		// We only fail if the down migration drops exactly that name.
		if strings.Contains(strings.ToLower(downSQL), "drop index if exists "+strings.ToLower(name)+"\n") ||
			strings.Contains(strings.ToLower(downSQL), "drop index if exists "+strings.ToLower(name)+";") {
			t.Errorf("down migration: must not drop unrelated index %s", name)
		}
	}

	// ── down: restores idx_sync_jobs_pending ─────────────────────────────────
	if !containsCI(downSQL, "CREATE INDEX IF NOT EXISTS idx_sync_jobs_pending") {
		t.Error("down migration: must restore idx_sync_jobs_pending")
	}
}

// readMigration reads an embedded migration file and returns its content.
func readMigration(t *testing.T, filename string) string {
	t.Helper()
	data, err := migrations.FS.ReadFile(filename)
	if err != nil {
		t.Fatalf("could not read embedded migration %s: %v", filename, err)
	}
	return string(data)
}

// containsCI reports whether s contains substr, case-insensitively.
func containsCI(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// extractIndexBlock returns the portion of sql starting from the first
// occurrence of indexName until the next semicolon (inclusive).
func extractIndexBlock(sql, indexName string) string {
	lower := strings.ToLower(sql)
	start := strings.Index(lower, strings.ToLower(indexName))
	if start == -1 {
		return ""
	}
	end := strings.Index(sql[start:], ";")
	if end == -1 {
		return sql[start:]
	}
	return sql[start : start+end+1]
}
