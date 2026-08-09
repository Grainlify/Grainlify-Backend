package handlers_test

import (
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// testDB returns a db.DB backed by a live pool from TEST_DB_URL, migrated to
// the current schema. Tests that need a real Postgres call this first and
// get skipped (not failed) when TEST_DB_URL isn't set, so `go test ./...`
// stays green on a machine with no local Postgres running. Thin wrapper
// around the shared internal/dbtest package so the many existing call sites
// in this package don't need to change.
func testDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.DB(t)
}
